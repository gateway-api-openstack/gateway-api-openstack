/*
Copyright 2026 The gateway-api-openstack Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package httproute

import (
	"context"
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

type routeGraphResult struct {
	bindingCheckpoint bool
	outcome           cloud.Outcome
}

type routeBindingCheckpointError struct {
	err error
}

func (e *routeBindingCheckpointError) Error() string {
	return e.err.Error()
}

func (e *routeBindingCheckpointError) Unwrap() error {
	return e.err
}

func (r *Reconciler) ensureRouteGraph(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
) (routeGraphResult, error) {
	if r.APIReader == nil {
		return routeGraphResult{}, controller.ErrAPIReaderRequired
	}
	if r.Writer == nil {
		return routeGraphResult{}, fmt.Errorf("gateway graph writer is required")
	}
	checkpoint, pending, err := r.prepareRouteGraphBinding(ctx, expectedRoute, expectedGateway)
	if err != nil {
		return routeGraphResult{}, err
	}
	if checkpoint || pending {
		return routeGraphResult{bindingCheckpoint: checkpoint}, nil
	}
	result, err := r.Writer.ReconcileGraph(ctx, graph.Request{
		Identity: controller.RouteIdentity(r.Config, expectedGateway, expectedRoute),
		Action:   graph.Ensure, Gateway: expectedGateway, Route: expectedRoute,
	})
	if err != nil {
		return routeGraphResult{}, err
	}
	expectedIdentity := controller.RouteIdentity(r.Config, expectedGateway, expectedRoute)
	var component *cloud.RouteGraphResult
	for i := range result.Routes {
		route := &result.Routes[i]
		if route.Identity.RouteUID != expectedIdentity.RouteUID {
			continue
		}
		if route.Identity != expectedIdentity || component != nil {
			return routeGraphResult{}, fmt.Errorf("provider returned an inconsistent HTTPRoute component identity")
		}
		component = route
	}
	if component != nil {
		if err := component.Result.Outcome.Validate(); err != nil {
			return routeGraphResult{}, fmt.Errorf("validate HTTPRoute component outcome: %w", err)
		}
		return routeGraphResult{outcome: component.Result.Outcome}, nil
	}
	if result.Outcome.State == cloud.OutcomeProgressing {
		return routeGraphResult{outcome: result.Outcome}, nil
	}
	return routeGraphResult{}, fmt.Errorf("provider omitted the ensured HTTPRoute component result")
}

// prepareRouteGraphBinding persists the binding before the writer can create resources.
func (r *Reconciler) prepareRouteGraphBinding(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (checkpoint, pending bool, retErr error) {
	release, err := graph.Acquire(ctx, r.Coordinator, string(gateway.UID))
	if err != nil {
		return false, false, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()
	snapshot, err := r.buildRouteValidationSnapshot(ctx, route, gateway, routeSnapshotForEnsure)
	if err != nil {
		return false, false, err
	}
	switch snapshot.disposition {
	case routeValidationGatewayPending:
		return false, true, nil
	case routeValidationBindingRequired:
		if _, err := r.bindRouteFromSnapshot(ctx, route, gateway, snapshot); err != nil {
			return false, false, &routeBindingCheckpointError{err: err}
		}
		return true, false, nil
	case routeValidationReady:
		return false, false, nil
	default:
		return false, false, fmt.Errorf("invalid HTTPRoute validation disposition %d", snapshot.disposition)
	}
}
