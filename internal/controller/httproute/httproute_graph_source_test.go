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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestRouteGraphInventoryRetainsBoundFragmentsWithoutParentRefs(t *testing.T) {
	cfg := testConfig()
	boundGateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1"}}
	binder := &Reconciler{Config: cfg}
	boundRoute := func(name, lifecycle string) *gatewayv1.HTTPRoute {
		route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: boundGateway.Namespace, Name: name, UID: types.UID(name + "-uid"), Generation: 1}}
		binder.applyRouteBinding(route, boundGateway)
		route.Annotations[cfg.RouteLifecycleAnnotation()] = lifecycle
		return route
	}
	detaching := boundRoute("a-detaching", controller.RouteDetaching)
	attached := boundRoute("b-attached", controller.RouteAttached)
	candidate := mapperTestHTTPRoute("default", "candidate", gatewayv1.ParentReference{Name: "edge"}, "backend")
	candidate.UID = "candidate-uid"
	unrelated := mapperTestHTTPRoute("default", "unrelated", gatewayv1.ParentReference{Name: "other"}, "backend")
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(attached, candidate, unrelated, detaching).Build()
	r := &Reconciler{Config: cfg, APIReader: kubeClient}
	identity := controller.RouteIdentity(cfg, boundGateway, detaching)
	input, err := r.ObserveRouteGraph(context.Background(), graph.Request{Identity: identity, Action: graph.DeleteRoute, Route: detaching}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []cloud.RouteGraphNode{
		{Disposition: cloud.GraphDelete, Spec: cloud.RouteSpec{Identity: identity}},
		{Disposition: cloud.GraphPreserve, Spec: cloud.RouteSpec{Identity: controller.RouteIdentity(cfg, boundGateway, attached)}},
	}
	if !reflect.DeepEqual(input.Nodes, want) {
		t.Fatalf("route graph = %#v, want detached fragment deletion and attached fragment preservation", input.Nodes)
	}
	if err := input.Validate(context.Background()); err != nil {
		t.Fatalf("unchanged inventory fence: %v", err)
	}
}

func TestRouteGraphInventoryFenceDetectsNewCandidatesAndLifecycleChanges(t *testing.T) {
	for _, change := range []string{"candidate", "bound fragment", "lifecycle", "unrelated route"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig()
			gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1"}}
			route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "route", UID: "route-1", Generation: 1}}
			r := &Reconciler{Config: cfg}
			r.applyRouteBinding(route, gateway)
			route.Annotations[cfg.RouteLifecycleAnnotation()] = controller.RouteDetaching
			kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(route).Build()
			r.APIReader = kubeClient
			input, err := r.ObserveRouteGraph(ctx, graph.Request{Identity: controller.RouteIdentity(cfg, gateway, route), Action: graph.DeleteRoute, Route: route}, nil)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "lifecycle":
				current := &gatewayv1.HTTPRoute{}
				if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(route), current); err != nil {
					t.Fatal(err)
				}
				current.Annotations[cfg.RouteLifecycleAnnotation()] = controller.RouteAttached
				if err := kubeClient.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			default:
				created := mapperTestHTTPRoute("default", "new-route", gatewayv1.ParentReference{Name: "edge"}, "backend")
				created.UID = "new-route-uid"
				switch change {
				case "bound fragment":
					created.Spec.ParentRefs = nil
					r.applyRouteBinding(created, gateway)
				case "unrelated route":
					created.Spec.ParentRefs[0].Name = "other"
				}
				if err := kubeClient.Create(ctx, created); err != nil {
					t.Fatal(err)
				}
			}
			err = input.Validate(ctx)
			if change == "unrelated route" {
				if err != nil {
					t.Fatalf("unrelated route invalidated graph: %v", err)
				}
			} else if !errors.Is(err, errHTTPRouteChanged) {
				t.Fatalf("fence error = %v, want changed graph input", err)
			}
		})
	}
}

func TestRouteGraphRejectsUnknownBoundFragmentLifecycle(t *testing.T) {
	cfg := testConfig()
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1"}}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "route", UID: "route-1", Generation: 1}}
	r := &Reconciler{Config: cfg}
	r.applyRouteBinding(route, gateway)
	delete(route.Annotations, cfg.RouteLifecycleAnnotation())
	r.APIReader = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(route).Build()
	_, err := r.ObserveRouteGraph(context.Background(), graph.Request{Identity: controller.RouteIdentity(cfg, gateway, route), Action: graph.DeleteRoute, Route: route}, nil)
	if !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("error = %v, want ownership conflict", err)
	}
}
