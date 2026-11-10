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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

func (r *Reconciler) isSelectedRoute(ctx context.Context, route *gatewayv1.HTTPRoute, parent managedRouteParent) (bool, error) {
	return r.isSelectedRouteWithReader(ctx, r.Client, route, parent, true)
}

func (r *Reconciler) isSelectedRouteWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	parent managedRouteParent,
	useCacheIndex bool,
) (bool, error) {
	var routes gatewayv1.HTTPRouteList
	listOptions := []client.ListOption{client.InNamespace(route.Namespace)}
	if useCacheIndex {
		listOptions = append(listOptions, client.MatchingFields{
			controller.IndexHTTPRouteByParentGateway: controller.ObjectKeyString(client.ObjectKeyFromObject(parent.gateway)),
		})
	}
	if err := reader.List(ctx, &routes, listOptions...); err != nil {
		return false, fmt.Errorf("list HTTPRoutes for attachment selection: %w", err)
	}
	return selectRouteCandidate(route, parent, routes.Items, func(candidate *gatewayv1.HTTPRoute) (routeSlotDecision, error) {
		return r.evaluateRouteSlotWithReader(ctx, reader, candidate, parent.gateway)
	})
}

func selectRouteCandidate(route *gatewayv1.HTTPRoute, parent managedRouteParent, routes []gatewayv1.HTTPRoute, evaluate func(*gatewayv1.HTTPRoute) (routeSlotDecision, error)) (bool, error) {
	winner := route
	for index := range routes {
		candidate := &routes[index]
		if candidate.Name == route.Name || !candidate.DeletionTimestamp.IsZero() || len(candidate.Spec.ParentRefs) != 1 {
			continue
		}
		candidateParent := managedRouteParent{ref: candidate.Spec.ParentRefs[0], gateway: parent.gateway, listener: parent.listener}
		if !controller.SameGatewayTarget(candidateParent.ref, candidate.Namespace, parent.ref, route.Namespace) ||
			validateRouteParent(candidate, candidateParent) != nil || !structurallySupportedRoute(candidate) {
			continue
		}
		decision, err := evaluate(candidate)
		if err != nil {
			return false, err
		}
		if !decision.canReserve {
			continue
		}
		if routePrecedes(candidate, winner) {
			winner = candidate
		}
	}
	return winner.Name == route.Name, nil
}

type routeSlotDecision struct {
	canReserve bool
	rejection  error
}

// evaluateRouteSlotWithReader excludes rejected Service profiles using the
// Gateway's frozen mode. Missing dependencies retain deterministic selection.
func (r *Reconciler) evaluateRouteSlotWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
) (routeSlotDecision, error) {
	if !controller.HasGatewayTopologyBinding(r.Config, gateway) {
		return routeSlotDecision{canReserve: true}, nil
	}
	topology, err := controller.StoredGatewayTopology(r.Config, gateway)
	if err != nil {
		return routeSlotDecision{}, err
	}
	backend := route.Spec.Rules[0].BackendRefs[0]
	var service corev1.Service
	if err := reader.Get(ctx, types.NamespacedName{Namespace: route.Namespace, Name: string(backend.Name)}, &service); err != nil {
		if apierrors.IsNotFound(err) {
			return routeSlotDecision{canReserve: true}, nil
		}
		return routeSlotDecision{}, fmt.Errorf("get candidate backend Service: %w", err)
	}
	return routeSlotForService(&service, *backend.Port, topology.Topology.MemberMode), nil
}

func routeSlotForService(service *corev1.Service, port gatewayv1.PortNumber, mode cloud.MemberMode) routeSlotDecision {
	if service == nil {
		return routeSlotDecision{canReserve: true}
	}
	if _, err := servicePortForBackend(service, port, mode); err != nil {
		var buildError *routeBuildError
		if errors.As(err, &buildError) && buildError.kind == routeErrorPending {
			return routeSlotDecision{canReserve: true}
		}
		return routeSlotDecision{rejection: err}
	}
	return routeSlotDecision{canReserve: true}
}

func routePrecedes(left, right *gatewayv1.HTTPRoute) bool {
	if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
		return left.CreationTimestamp.Before(&right.CreationTimestamp)
	}
	return left.Namespace+"/"+left.Name < right.Namespace+"/"+right.Name
}
