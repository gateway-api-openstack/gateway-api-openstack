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
	"reflect"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

// ObserveRouteGraph rebuilds every bound fragment from a complete namespace read.
// Cached indexes cannot establish completeness for a destructive cloud plan.
func (r *Reconciler) ObserveRouteGraph(ctx context.Context, request graph.Request, gateway *gatewayv1.Gateway) (graph.RouteInputs, error) {
	if r.APIReader == nil {
		return graph.RouteInputs{}, controller.ErrAPIReaderRequired
	}
	routes, err := r.liveGraphRoutes(ctx, request.Identity)
	if err != nil {
		return graph.RouteInputs{}, err
	}
	input := graph.RouteInputs{Nodes: make([]cloud.RouteGraphNode, 0, len(routes))}
	fences := make([]func(context.Context) error, 0, len(routes))
	foundRequested := request.Route == nil
	for i := range routes {
		route := &routes[i]
		identity, present, err := r.storedRouteIdentity(route)
		if err != nil {
			return graph.RouteInputs{}, err
		}
		if !present || !controller.SameGatewayIdentity(identity, request.Identity) {
			continue
		}
		if request.Route != nil && route.UID == request.Route.UID {
			foundRequested = true
			if !sameHTTPRouteRevision(route, request.Route.UID, request.Route.Generation, !request.Route.DeletionTimestamp.IsZero()) {
				return graph.RouteInputs{}, errHTTPRouteChanged
			}
		}
		node := cloud.RouteGraphNode{Spec: cloud.RouteSpec{Identity: identity}, Disposition: cloud.GraphPreserve}
		lifecycle := route.Annotations[r.Config.RouteLifecycleAnnotation()]
		switch lifecycle {
		case controller.RouteAttached, controller.RouteDetaching:
		default:
			return graph.RouteInputs{}, fmt.Errorf("%w: HTTPRoute fragment lifecycle is missing or invalid", cloud.ErrOwnershipConflict)
		}
		if request.Action == graph.DeleteGateway {
			input.Nodes = append(input.Nodes, node)
			continue
		}
		if lifecycle == controller.RouteDetaching {
			node.Disposition = cloud.GraphDelete
			snapshot := routeCleanupSnapshot{route: route, identity: identity, disposition: routeCleanupRequired}
			fences = append(fences, func(ctx context.Context) error { return r.sealRouteCleanupSnapshot(ctx, snapshot) })
		} else if request.Action == graph.Ensure {
			if gateway == nil {
				return graph.RouteInputs{}, errors.New("gateway is required for an attached route")
			}
			snapshot, err := r.buildRouteValidationSnapshot(ctx, route, gateway, routeSnapshotForEnsure)
			if err != nil {
				return graph.RouteInputs{}, &cloud.GraphError{Identity: identity, Err: err}
			}
			if snapshot.disposition != routeValidationReady {
				return graph.RouteInputs{}, &cloud.GraphError{Identity: identity, Err: errHTTPRouteChanged}
			}
			node.Disposition, node.Spec = cloud.GraphEnsure, snapshot.desired
			fences = append(fences, func(ctx context.Context) error {
				return r.sealRouteMutationSnapshot(ctx, snapshot, routeBindingRequired)
			})
		}
		if request.Action == graph.DeleteRoute && request.Route != nil && route.UID == request.Route.UID && node.Disposition != cloud.GraphDelete {
			return graph.RouteInputs{}, errHTTPRouteChanged
		}
		input.Nodes = append(input.Nodes, node)
	}
	if !foundRequested {
		return graph.RouteInputs{}, errHTTPRouteChanged
	}
	input.Validate = func(ctx context.Context) error {
		current, err := r.liveGraphRoutes(ctx, request.Identity)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(routeGraphRevisions(routes), routeGraphRevisions(current)) {
			return errHTTPRouteChanged
		}
		for _, fence := range fences {
			if err := fence(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	return input, nil
}

func (r *Reconciler) liveGraphRoutes(ctx context.Context, identity cloud.Identity) ([]gatewayv1.HTTPRoute, error) {
	var list gatewayv1.HTTPRouteList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(identity.GatewayNamespace)); err != nil {
		return nil, fmt.Errorf("list Gateway route fragments: %w", err)
	}
	result := make([]gatewayv1.HTTPRoute, 0)
	for _, route := range list.Items {
		boundUID := route.Annotations[r.Config.RouteGatewayUIDAnnotation()]
		if boundUID == identity.GatewayUID {
			result = append(result, route)
			continue
		}
		// An unbound candidate can change route selection while a graph is built.
		for _, parent := range route.Spec.ParentRefs {
			namespace := route.Namespace
			if parent.Namespace != nil {
				namespace = string(*parent.Namespace)
			}
			if controller.IsGatewayParentRef(parent) && namespace == identity.GatewayNamespace && string(parent.Name) == identity.GatewayName {
				result = append(result, route)
				break
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func routeGraphRevisions(routes []gatewayv1.HTTPRoute) []string {
	revisions := make([]string, 0, len(routes))
	for _, route := range routes {
		revisions = append(revisions, string(route.UID)+"/"+route.ResourceVersion)
	}
	return revisions
}
