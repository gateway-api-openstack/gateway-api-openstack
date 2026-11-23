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

package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func forwardingInput(routes ...gatewayv1.HTTPRoute) Input {
	service := backendService("backend")
	service.Namespace = "apps"
	return Input{Gateway: logicalGateway(), Routes: routes, Services: []corev1.Service{service}}
}

func TestForwardingGroupsLogicalListenersWithoutHostSlotCollisions(t *testing.T) {
	route := logicalRoute("api", "/api")
	route.Spec.Hostnames = []gatewayv1.Hostname{"b.example.com", "a.example.com"}
	input := forwardingInput(route)
	input.Gateway.Spec.Listeners = []gatewayv1.Listener{
		{Name: "a", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptr.To(gatewayv1.Hostname("a.example.com"))},
		{Name: "b", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptr.To(gatewayv1.Hostname("b.example.com"))},
		{Name: "wildcard", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptr.To(gatewayv1.Hostname("*.example.com"))},
		{Name: "other-port", Port: 8080, Protocol: gatewayv1.HTTPProtocolType},
	}
	logical := compileLogical(t, input)
	before, err := json.Marshal(logical)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LowerHTTPForwarding(logical)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Listeners) != 2 || len(got.Routes) != 1 || len(got.Routes[0].Rules) != 1 {
		t.Fatalf("physical listener/rule grouping = %#v", got)
	}
	matches := got.Routes[0].Rules[0].Spec.Matches
	if len(matches) != 4 {
		t.Fatalf("got %d matches, want two hosts on each physical listener: %#v", len(matches), matches)
	}
	for i, match := range matches {
		port := 80
		if i >= 2 {
			port = 8080
		}
		if match.Listener != (cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: port}) || match.HostIndex != i%2 || match.Priority != i%2 {
			t.Fatalf("host slot or priority collision: %#v", matches)
		}
	}
	after, err := json.Marshal(logical)
	if err != nil || string(before) != string(after) {
		t.Fatalf("lowering changed the logical input: %v", err)
	}
	slices.Reverse(input.Gateway.Spec.Listeners)
	slices.Reverse(input.Routes[0].Spec.Hostnames)
	permuted, err := LowerHTTPForwarding(compileLogical(t, input))
	if err != nil || !reflect.DeepEqual(got, permuted) {
		t.Fatalf("input order changed physical mapping: %v\n got=%#v\nwant=%#v", err, permuted, got)
	}
}

func TestForwardingPrioritiesInterleaveRulesAcrossRoutes(t *testing.T) {
	broad := logicalRoute("broad", "/")
	broad.Spec.Rules = append(broad.Spec.Rules, *broad.Spec.Rules[0].DeepCopy())
	broad.Spec.Rules[1].Matches[0].Path.Value = ptr.To("/api/private")
	specific := logicalRoute("specific", "/api")
	specific.Spec.Rules[0].Matches = append(specific.Spec.Rules[0].Matches, gatewayv1.HTTPRouteMatch{
		Path:    &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchExact), Value: ptr.To("/health")},
		Headers: []gatewayv1.HTTPHeaderMatch{{Name: "X-Tenant", Value: "one"}},
	})
	input := forwardingInput(specific, broad)
	got, err := LowerHTTPForwarding(compileLogical(t, input))
	if err != nil {
		t.Fatal(err)
	}
	priorities := make(map[string][]int)
	for _, route := range got.Routes {
		for _, rule := range route.Rules {
			for _, match := range rule.Spec.Matches {
				priorities[route.Source.Name] = append(priorities[route.Source.Name], match.Priority)
				if match.PathValue == "/health" && !slices.Equal(match.Headers, []cloud.HeaderMatch{{Name: "x-tenant", Value: "one"}}) {
					t.Fatalf("exact headers changed while lowering: %#v", match)
				}
			}
		}
	}
	if !slices.Equal(priorities["broad"], []int{3, 1}) || !slices.Equal(priorities["specific"], []int{0, 2}) {
		t.Fatalf("priorities must interleave across Routes and rules: %v", priorities)
	}
	slices.Reverse(input.Routes)
	permuted, err := LowerHTTPForwarding(compileLogical(t, input))
	if err != nil || !reflect.DeepEqual(got, permuted) {
		t.Fatalf("Route input order changed physical mapping: %v", err)
	}
}

func TestForwardingRejectsUnrepresentableRulesWithoutPartialPlan(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*gatewayv1.HTTPRoute)
	}{
		{name: "missing backend", change: func(r *gatewayv1.HTTPRoute) { r.Spec.Rules[0].BackendRefs[0].Name = "missing" }},
		{name: "multiple backends", change: func(r *gatewayv1.HTTPRoute) {
			r.Spec.Rules[0].BackendRefs = append(r.Spec.Rules[0].BackendRefs, r.Spec.Rules[0].BackendRefs[0])
		}},
		{name: "zero weight", change: func(r *gatewayv1.HTTPRoute) { r.Spec.Rules[0].BackendRefs[0].Weight = ptr.To(int32(0)) }},
		{name: "header modifier", change: func(r *gatewayv1.HTTPRoute) {
			r.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "x-version", Value: "v2"}}}}}
		}},
		{name: "unsupported predicate", change: func(r *gatewayv1.HTTPRoute) { r.Spec.Rules[0].Matches[0].Method = ptr.To(gatewayv1.HTTPMethodGet) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := logicalRoute("specific", "/api")
			test.change(&route)
			got, err := LowerHTTPForwarding(compileLogical(t, forwardingInput(logicalRoute("broad", "/"), route)))
			var diagnostic *Diagnostic
			if !errors.As(err, &diagnostic) || !reflect.DeepEqual(got, HTTPForwarding{}) {
				t.Fatalf("unsupported rule produced a partial forwarding plan: %#v, %v", got, err)
			}
		})
	}
}

func TestForwardingIgnoresUnattachedRouteWithoutChangingAcceptedTraffic(t *testing.T) {
	accepted := logicalRoute("accepted", "/")
	rejected := logicalRoute("unattached", "/private")
	rejected.Spec.ParentRefs[0].SectionName = ptr.To(gatewayv1.SectionName("missing"))
	rejected.Spec.Rules[0].BackendRefs[0].Name = "missing"
	got, err := LowerHTTPForwarding(compileLogical(t, forwardingInput(accepted, rejected)))
	if err != nil || len(got.Routes) != 1 || got.Routes[0].Source.Name != accepted.Name {
		t.Fatalf("unattached Route affected accepted forwarding: %#v, %v", got, err)
	}
}

func TestForwardingIncludesEmptyListeners(t *testing.T) {
	input := forwardingInput()
	input.Gateway.Spec.Listeners = append(input.Gateway.Spec.Listeners, gatewayv1.Listener{Name: "other", Port: 8080, Protocol: gatewayv1.HTTPProtocolType})
	got, err := LowerHTTPForwarding(compileLogical(t, input))
	if err != nil || len(got.Listeners) != 2 || len(got.Routes) != 0 {
		t.Fatalf("empty listener mapping = %#v, %v", got, err)
	}
}

func TestForwardingPreservesCrossNamespaceAuthorization(t *testing.T) {
	route := logicalRoute("frontend", "/")
	route.Namespace = "frontend"
	route.Spec.ParentRefs[0].Namespace = ptr.To(gatewayv1.Namespace("apps"))
	route.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("backend"))
	input := forwardingInput(route)
	input.Gateway.Spec.Listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)}}
	input.Services[0].Namespace = "backend"
	input.ReferenceGrants = []gatewayv1.ReferenceGrant{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "backend", Name: "allow-frontend"},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "frontend"}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1.ObjectName("backend"))}},
		},
	}}
	got, err := LowerHTTPForwarding(compileLogical(t, input))
	if err != nil || len(got.Routes) != 1 || got.Routes[0].Rules[0].Backend.Service.Namespace != "backend" {
		t.Fatalf("authorized cross-namespace forwarding = %#v, %v", got, err)
	}
	input.ReferenceGrants = nil
	got, err = LowerHTTPForwarding(compileLogical(t, input))
	var diagnostic *Diagnostic
	if !errors.As(err, &diagnostic) || diagnostic.Reason != string(gatewayv1.RouteReasonRefNotPermitted) || !reflect.DeepEqual(got, HTTPForwarding{}) {
		t.Fatalf("revoked reference produced a forwarding plan: %#v, %v", got, err)
	}
}

func TestForwardingUnionsSameGatewayParentAttachments(t *testing.T) {
	route := logicalRoute("api", "/")
	route.Spec.Hostnames = []gatewayv1.Hostname{"api.example.com"}
	route.Spec.ParentRefs = []gatewayv1.ParentReference{
		{Name: "shop", SectionName: ptr.To(gatewayv1.SectionName("exact"))},
		{Name: "shop", SectionName: ptr.To(gatewayv1.SectionName("wildcard"))},
	}
	input := forwardingInput(route)
	input.Gateway.Spec.Listeners = []gatewayv1.Listener{
		{Name: "exact", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptr.To(gatewayv1.Hostname("api.example.com"))},
		{Name: "wildcard", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptr.To(gatewayv1.Hostname("*.example.com"))},
	}
	logical := compileLogical(t, input)
	got, err := LowerHTTPForwarding(logical)
	if err != nil || len(got.Routes) != 1 || len(got.Routes[0].Rules[0].Spec.Matches) != 1 {
		t.Fatalf("parent attachments duplicated a physical match: %#v, %v", got, err)
	}
	for _, listener := range logical.Listeners {
		if listener.AttachedRoutes != 1 {
			t.Fatalf("listener count = %d, want one distinct Route", listener.AttachedRoutes)
		}
	}
	input.Routes[0].Spec.ParentRefs = input.Routes[0].Spec.ParentRefs[:1]
	after, err := LowerHTTPForwarding(compileLogical(t, input))
	if err != nil || !reflect.DeepEqual(got, after) {
		t.Fatalf("removing a redundant attachment changed forwarding: %v", err)
	}
}

func TestForwardingPreservesValidListenersWhenSiblingsAreRejected(t *testing.T) {
	input := forwardingInput(logicalRoute("route", "/"))
	input.Gateway.Spec.Listeners = []gatewayv1.Listener{
		{Name: "first", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
		{Name: "survivor", Protocol: gatewayv1.HTTPProtocolType, Port: 8080},
	}
	before, err := LowerHTTPForwarding(compileLogical(t, input))
	if err != nil {
		t.Fatal(err)
	}
	input.Gateway.Spec.Listeners = append(input.Gateway.Spec.Listeners,
		gatewayv1.Listener{Name: "conflict", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
		gatewayv1.Listener{Name: "unsupported", Protocol: gatewayv1.TCPProtocolType, Port: 8080},
	)
	after, err := LowerHTTPForwarding(compileLogical(t, input))
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Listeners) != 1 || after.Listeners[0].Key.Port != 8080 || len(after.Routes) != 1 {
		t.Fatalf("invalid listeners displaced the survivor: %#v", after)
	}
	matches := after.Routes[0].Rules[0].Spec.Matches
	original := before.Routes[0].Rules[0].Spec.Matches
	if len(matches) != 1 || !reflect.DeepEqual(matches[0], original[1]) {
		t.Fatalf("surviving match changed: got %#v, before %#v", matches, original)
	}
	input.Gateway.Spec.Listeners = append(input.Gateway.Spec.Listeners[:1], input.Gateway.Spec.Listeners[2:]...)
	if _, err := LowerHTTPForwarding(compileLogical(t, input)); err == nil {
		t.Fatal("Gateway without a valid HTTP listener produced a plan")
	}
}
