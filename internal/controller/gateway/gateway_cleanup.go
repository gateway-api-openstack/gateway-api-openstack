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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

// cleanupGateway confirms the owned graph is absent before releasing its binding
// or preparing a replacement. A managed Gateway retains its frozen topology for
// its lifetime, including periods when its spec cannot be provisioned.
func (r *Reconciler) cleanupGateway(ctx context.Context, scope *gatewayScope) (ctrl.Result, error) {
	expectedResourceVersion := scope.gateway.ResourceVersion
	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, client.ObjectKeyFromObject(scope.gateway), &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			scope.skipPatch = true
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if expectedResourceVersion != "" && gateway.ResourceVersion != expectedResourceVersion {
		return ctrl.Result{}, apierrors.NewConflict(
			schema.GroupResource{Group: gatewayv1.GroupName, Resource: "gateways"},
			gateway.Name,
			errors.New("gateway changed before OpenStack resource cleanup"),
		)
	}
	if err := scope.refresh(&gateway); err != nil {
		return ctrl.Result{}, err
	}
	if !controller.GatewayHasControllerBinding(r.Config, scope.gateway) {
		return ctrl.Result{}, nil
	}
	if err := controller.ValidateGatewayBinding(r.Config, scope.gateway); err != nil {
		return ctrl.Result{}, err
	}
	outcome, err := r.deleteGateway(ctx, scope.gateway)
	if err != nil {
		if errors.Is(err, controller.ErrUnsupportedGatewayAPIVersion) {
			return r.setGatewayUnsupportedVersion(ctx, scope)
		}
		if errors.Is(err, errGatewayChanged) {
			return ctrl.Result{}, err
		}
		providerErr := fmt.Errorf("delete OpenStack resources for Gateway: %w", err)
		return r.handleGatewayDeleteFailure(scope, providerErr)
	}
	if err := outcome.Validate(); err != nil {
		return ctrl.Result{}, fmt.Errorf("validate Gateway deletion outcome: %w", err)
	}
	if outcome.State == cloud.OutcomeProgressing {
		return ctrl.Result{RequeueAfter: controller.ProviderProgressRequeueAfter(outcome, scope.gateway.UID)}, nil
	}
	// Recheck the lifecycle after cloud observation. A concurrent spec or class
	// change must not erase the snapshot or authorize a new listener binding.
	snapshot, err := r.observeGatewayCleanupSnapshot(ctx, scope.gateway)
	if err != nil {
		return ctrl.Result{}, err
	}
	if snapshot.disposition == gatewayCleanupHold {
		return ctrl.Result{}, errGatewayChanged
	}
	if snapshot.disposition == gatewayCleanupRebuild {
		if snapshot.listenerPort == "" {
			return ctrl.Result{}, nil
		}
		scope.gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] = snapshot.listenerPort
		if err := r.patchGatewayMetadata(ctx, scope); err != nil {
			return ctrl.Result{}, fmt.Errorf("checkpoint Gateway graph replacement: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	controller.ClearGatewayTopology(r.Config, scope.gateway)
	controllerutil.RemoveFinalizer(scope.gateway, r.Config.GatewayFinalizer())
	delete(scope.gateway.Annotations, r.Config.GatewayListenerPortAnnotation())
	delete(scope.gateway.Annotations, r.Config.GatewayClusterIDAnnotation())
	delete(scope.gateway.Annotations, r.Config.GatewayProjectIDAnnotation())
	if err := r.patchGatewayMetadata(ctx, scope); err != nil {
		if apierrors.IsNotFound(err) {
			scope.skipPatch = true
		} else {
			return ctrl.Result{}, fmt.Errorf("remove Gateway resource binding: %w", err)
		}
	}
	if !scope.gateway.DeletionTimestamp.IsZero() {
		scope.skipPatch = true
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) deleteGateway(ctx context.Context, expected *gatewayv1.Gateway) (cloud.Outcome, error) {
	if r.Writer == nil {
		return cloud.Outcome{}, fmt.Errorf("gateway graph writer is required")
	}
	result, err := r.Writer.ReconcileGraph(ctx, graph.Request{Identity: controller.StoredGatewayIdentity(r.Config, expected), Action: graph.DeleteGateway, Gateway: expected})
	return result.Outcome, err
}
