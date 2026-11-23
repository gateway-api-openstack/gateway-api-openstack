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

package controller

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func TestRouteBindingsPreserveIndependentGatewayOwnership(t *testing.T) {
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "routes", Name: "api", UID: "route-uid", Finalizers: []string{"foreign.example/finalizer"}}}
	first := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gateways", Name: "first", UID: "gateway-a"}}
	second := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gateways", Name: "second", UID: "gateway-b"}}
	for _, gateway := range []*gatewayv1.Gateway{second, first} {
		if err := SetRouteBinding(cfg, route, RouteBinding{Identity: RouteIdentity(cfg, gateway, route), Lifecycle: RouteAttached}); err != nil {
			t.Fatal(err)
		}
	}
	bindings, err := StoredRouteBindings(cfg, route)
	if err != nil || len(bindings) != 2 || bindings[0].Identity.GatewayUID != "gateway-a" {
		t.Fatalf("bindings=%+v error=%v", bindings, err)
	}
	firstBinding := bindings[0]
	firstBinding.Lifecycle = RouteDetaching
	if err := SetRouteBinding(cfg, route, firstBinding); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRouteBinding(cfg, route, "gateway-a"); err != nil {
		t.Fatal(err)
	}
	remaining, err := StoredRouteBindings(cfg, route)
	if err != nil || len(remaining) != 1 || remaining[0] != bindings[1] {
		t.Fatalf("remaining=%+v error=%v", remaining, err)
	}
	if !slices.Contains(route.Finalizers, cfg.RouteFinalizer()) || !slices.Contains(route.Finalizers, "foreign.example/finalizer") {
		t.Fatalf("finalizers=%v", route.Finalizers)
	}
	if err := RemoveRouteBinding(cfg, route, "gateway-b"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(route.Finalizers, []string{"foreign.example/finalizer"}) || route.Annotations[cfg.RouteBindingsAnnotation()] != "" {
		t.Fatalf("cleanup metadata=%+v", route.ObjectMeta)
	}
}

func TestRouteBindingsRejectConflictingOrIncompleteMetadata(t *testing.T) {
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "routes", Name: "api", UID: "route-uid"}}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gateways", Name: "edge", UID: "gateway-uid"}}
	binding := RouteBinding{Identity: RouteIdentity(cfg, gateway, route), Lifecycle: RouteAttached}
	if err := SetRouteBinding(cfg, route, binding); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*gatewayv1.HTTPRoute, *[]RouteBinding)
	}{
		{name: "duplicate Gateway", mutate: func(_ *gatewayv1.HTTPRoute, b *[]RouteBinding) { *b = append(*b, (*b)[0]) }},
		{name: "copied Route UID", mutate: func(r *gatewayv1.HTTPRoute, _ *[]RouteBinding) { r.UID = "replacement-route" }},
		{name: "different project", mutate: func(_ *gatewayv1.HTTPRoute, b *[]RouteBinding) {
			(*b)[0].Identity.OpenStackProjectID = "foreign-project"
		}},
		{name: "different controller", mutate: func(_ *gatewayv1.HTTPRoute, b *[]RouteBinding) {
			(*b)[0].Identity.Controller = "foreign.example/controller"
		}},
		{name: "missing finalizer", mutate: func(r *gatewayv1.HTTPRoute, _ *[]RouteBinding) { r.Finalizers = []string{cfg.RouteFinalizer()} }},
		{name: "extra finalizer", mutate: func(r *gatewayv1.HTTPRoute, _ *[]RouteBinding) {
			r.Finalizers = append(r.Finalizers, cfg.RouteBindingFinalizerPrefix()+"unknown")
		}},
		{name: "missing lifecycle", mutate: func(_ *gatewayv1.HTTPRoute, b *[]RouteBinding) { (*b)[0].Lifecycle = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := route.DeepCopy()
			bindings := []RouteBinding{binding}
			test.mutate(candidate, &bindings)
			encoded, err := json.Marshal(bindings)
			if err != nil {
				t.Fatal(err)
			}
			candidate.Annotations[cfg.RouteBindingsAnnotation()] = string(encoded)
			if _, err := StoredRouteBindings(cfg, candidate); !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestSetRouteBindingDoesNotMutateRejectedInput(t *testing.T) {
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "routes", Name: "api", UID: "route-uid"}}
	before := route.DeepCopy()
	invalid := RouteBinding{Identity: cloud.Identity{GatewayUID: "gateway-uid"}, Lifecycle: RouteAttached}
	if err := SetRouteBinding(cfg, route, invalid); !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(before, route) {
		t.Fatal("rejected binding changed the input object")
	}
}

func TestRouteBindingReleaseVersionIsTraceMetadata(t *testing.T) {
	cfg := testConfig()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "routes", Name: "api", UID: "route-uid"}}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gateways", Name: "edge", UID: "gateway-uid"}}
	if err := SetRouteBinding(cfg, route, RouteBinding{Identity: RouteIdentity(cfg, gateway, route), Lifecycle: RouteAttached}); err != nil {
		t.Fatal(err)
	}
	cfg.ControllerVersion = "next-release"
	binding, present, err := StoredRouteBinding(cfg, route, string(gateway.UID))
	if err != nil || !present || binding.Identity.ControllerVersion != cfg.ControllerVersion {
		t.Fatalf("binding=%+v present=%t error=%v", binding, present, err)
	}
}
