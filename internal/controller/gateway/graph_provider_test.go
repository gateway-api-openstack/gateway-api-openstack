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

package gateway

import (
	"context"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// legacyTestProvider preserves existing component assertions while the tests
// exercise the complete production graph writer and its Kubernetes fences.
type legacyTestProvider interface {
	EnsureGateway(context.Context, cloud.GatewaySpec) (cloud.GatewayResult, error)
	DeleteGateway(context.Context, cloud.Identity) (cloud.Outcome, error)
	EnsureRoute(context.Context, cloud.RouteSpec) (cloud.RouteResult, error)
	DeleteRoute(context.Context, cloud.Identity) (cloud.Outcome, error)
}

func fakeGraphResult(ctx context.Context, p legacyTestProvider, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	result := cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome(), Gateway: cloud.GatewayReadyResult(cloud.GatewayState{LoadBalancerID: "lb-1", VIPAddress: "192.0.2.10", ListenerID: "listener-1"})}
	var err error
	if spec.Gateway.Disposition == cloud.GraphDelete {
		result.Outcome, err = p.DeleteGateway(ctx, spec.Identity)
		return result, err
	}
	for _, node := range spec.Routes {
		component := cloud.RouteReadyResult(cloud.RouteState{})
		switch node.Disposition {
		case cloud.GraphEnsure:
			component, err = p.EnsureRoute(ctx, node.Spec)
		case cloud.GraphDelete:
			component.Outcome, err = p.DeleteRoute(ctx, node.Spec.Identity)
		}
		result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: node.Spec.Identity, Result: component})
		if err != nil {
			return result, &cloud.GraphError{Identity: node.Spec.Identity, Err: err}
		}
		if component.Outcome.State == cloud.OutcomeProgressing {
			result.Outcome = component.Outcome
		}
	}
	if len(spec.Routes) == 0 && spec.Gateway.Disposition == cloud.GraphEnsure {
		result.Gateway, err = p.EnsureGateway(ctx, spec.Gateway.Spec)
		result.Outcome = result.Gateway.Outcome
	}
	return result, err
}

func (p *recordingProvider) ReconcileGatewayGraph(ctx context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	return fakeGraphResult(ctx, p, spec)
}

func newTestReconciler(value Reconciler, provider cloud.GraphProvider) *Reconciler {
	r := &value
	if r.TopologyResolver == nil {
		r.TopologyResolver, _ = provider.(cloud.TopologyResolver)
	}
	if r.Writer == nil {
		r.Writer = &graph.Writer{Coordinator: r.Coordinator, Gateway: r, Routes: emptyTestRoutes{}, Provider: provider}
	}
	return r
}

type emptyTestRoutes struct{}

func (emptyTestRoutes) ObserveRouteGraph(context.Context, graph.Request, *gatewayv1.Gateway) (graph.RouteInputs, error) {
	return graph.RouteInputs{}, nil
}
