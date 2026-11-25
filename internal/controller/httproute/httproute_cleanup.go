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
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

// detachRoute removes all recorded fragments, preserving each failing binding.
func (r *Reconciler) detachRoute(ctx context.Context, route *gatewayv1.HTTPRoute) (ctrl.Result, error) {
	scope := &httpRouteScope{route: route}
	return r.finalizeRoute(ctx, scope)
}

func (r *Reconciler) finalizeRoute(ctx context.Context, scope *httpRouteScope) (ctrl.Result, error) {
	if scope.identity.GatewayUID != "" {
		return r.finalizeRouteBinding(ctx, scope)
	}
	bindings, err := controller.StoredRouteBindings(r.Config, scope.route)
	if err != nil {
		return ctrl.Result{}, err
	}
	var result ctrl.Result
	var resultErr error
	for _, binding := range bindings {
		child := &httpRouteScope{route: scope.route, identity: binding.Identity}
		scope.children = append(scope.children, child)
		current, err := r.finalizeRouteBinding(ctx, child)
		result = mergeRouteResults(result, current)
		resultErr = errors.Join(resultErr, err)
	}
	return result, resultErr
}

func (r *Reconciler) finalizeRouteBinding(ctx context.Context, scope *httpRouteScope) (ctrl.Result, error) {
	route := scope.route
	current, stored, present, err := r.getRouteBinding(ctx, client.ObjectKeyFromObject(route), route.UID, route.Generation, !route.DeletionTimestamp.IsZero(), scope.identity.GatewayUID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if current == nil || !present {
		return ctrl.Result{}, nil
	}
	outcome, err := r.deleteRoute(ctx, current, stored)
	*route = *current
	if err != nil {
		return r.handleRouteFinalizationFailure(ctx, scope, stored, fmt.Errorf("delete HTTPRoute resources during finalization: %w", err))
	}
	if err := outcome.Validate(); err != nil {
		return ctrl.Result{}, fmt.Errorf("validate HTTPRoute finalization outcome: %w", err)
	}
	if outcome.State == cloud.OutcomeProgressing {
		if _, err := r.patchRouteCleanupFailure(ctx, route, stored.GatewayUID, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: controller.ProviderProgressRequeueAfter(outcome, route.UID)}, nil
	}
	if err := r.clearStoredRouteBinding(ctx, route, stored, true); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) patchRouteCleanupFailure(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	gatewayUID string,
	reason string,
) (bool, error) {
	routeKey := client.ObjectKeyFromObject(route)
	routeUID, generation, isDeleting := route.UID, route.Generation, !route.DeletionTimestamp.IsZero()
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changed = false
		current := &gatewayv1.HTTPRoute{}
		if err := r.routePatchReader().Get(ctx, routeKey, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !sameHTTPRouteRevision(current, routeUID, generation, isDeleting) {
			return errHTTPRouteChanged
		}
		binding, present, err := controller.StoredRouteBinding(r.Config, current, gatewayUID)
		if err != nil {
			return err
		}
		if !present {
			return errHTTPRouteChanged
		}
		if binding.CleanupFailure == reason {
			*route = *current
			return nil
		}
		base := current.DeepCopy()
		binding.CleanupFailure = reason
		if err := controller.SetRouteBinding(r.Config, current, binding); err != nil {
			return err
		}
		if err := r.Patch(ctx, current, controller.OptimisticMergeFrom(base)); err != nil {
			return client.IgnoreNotFound(err)
		}
		changed = true
		*route = *current
		return nil
	})
	return changed, err
}

func (r *Reconciler) deleteRoute(
	ctx context.Context,
	expected *gatewayv1.HTTPRoute,
	identity cloud.Identity,
) (cloud.Outcome, error) {
	if r.APIReader == nil {
		return cloud.Outcome{}, controller.ErrAPIReaderRequired
	}
	if r.Writer == nil {
		return cloud.Outcome{}, fmt.Errorf("gateway graph writer is required")
	}
	binding, present, err := controller.StoredRouteBinding(r.Config, expected, identity.GatewayUID)
	if err != nil {
		return cloud.Outcome{}, err
	}
	if !present {
		return cloud.Outcome{}, errHTTPRouteChanged
	}
	if binding.Lifecycle != controller.RouteDetaching {
		if err := r.checkpointRouteDetaching(ctx, expected, identity); err != nil {
			return cloud.Outcome{}, err
		}
		return cloud.ProgressingOutcome("HTTPRoute cleanup checkpoint was stored", time.Second), nil
	}
	result, err := r.Writer.ReconcileGraph(ctx, graph.Request{Identity: identity, Action: graph.DeleteRoute, Route: expected})
	return result.Outcome, err
}

func (r *Reconciler) checkpointRouteDetaching(ctx context.Context, expected *gatewayv1.HTTPRoute, identity cloud.Identity) error {
	release, err := graph.Acquire(ctx, r.Coordinator, identity.GatewayUID)
	if err != nil {
		return fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()
	snapshot, err := r.buildRouteCleanupSnapshot(ctx, expected, identity)
	if err != nil {
		return err
	}
	if snapshot.disposition != routeCleanupRequired {
		return errHTTPRouteChanged
	}
	if err := r.sealRouteCleanupSnapshot(ctx, snapshot); err != nil {
		return err
	}
	base := snapshot.route.DeepCopy()
	desired := snapshot.route.DeepCopy()
	binding, _, err := controller.StoredRouteBinding(r.Config, desired, identity.GatewayUID)
	if err != nil {
		return err
	}
	binding.Lifecycle = controller.RouteDetaching
	if err := controller.SetRouteBinding(r.Config, desired, binding); err != nil {
		return err
	}
	if err := r.Patch(ctx, desired, controller.OptimisticMergeFrom(base)); err != nil {
		return err
	}
	*expected = *desired
	return nil
}
