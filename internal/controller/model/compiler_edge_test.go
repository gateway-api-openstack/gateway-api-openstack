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
	"reflect"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestCompileParentRejectionDoesNotDependOnListenerOrder(t *testing.T) {
	gateway := edgeGateway()
	gateway.Spec.Listeners = []gatewayv1.Listener{
		{Name: "same-namespace", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
		{
			Name: "other-hostname", Port: 8080, Protocol: gatewayv1.HTTPProtocolType,
			Hostname: ptr.To(gatewayv1.Hostname("other.example.com")),
			AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
				From: ptr.To(gatewayv1.NamespacesFromAll),
			}},
		},
	}
	route := edgeRoute(gateway)
	route.Namespace = "application"
	route.Spec.Hostnames = []gatewayv1.Hostname{"app.example.com"}
	input := Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{route}}
	for iteration := 0; iteration < 2; iteration++ {
		compiled, err := Compile(input)
		if err != nil {
			t.Fatal(err)
		}
		parent := compiled.Routes[0].Parents[0]
		if parent.State != AttachmentRejected {
			t.Fatalf("parent state = %s, want rejected", parent.State)
		}
		if len(parent.Diagnostics) != 1 || parent.Diagnostics[0].Reason != string(gatewayv1.RouteReasonNoMatchingListenerHostname) {
			t.Fatalf("parent diagnostics = %#v, want NoMatchingListenerHostname", parent.Diagnostics)
		}
		slices.Reverse(gateway.Spec.Listeners)
	}
}

func TestCompileRejectsOnlyInvalidRules(t *testing.T) {
	gateway := edgeGateway()
	route := edgeRoute(gateway)
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{unsupportedEdgeRule()}
	compiled, err := Compile(Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{route}})
	if err != nil {
		t.Fatal(err)
	}
	parent := compiled.Routes[0].Parents[0]
	if parent.State != AttachmentRejected || len(parent.Diagnostics) == 0 {
		t.Fatalf("parent = %#v, want rejection explaining the unsupported rule", parent)
	}
	if compiled.Listeners[0].AttachedRoutes != 0 {
		t.Fatalf("attached routes = %d, want zero for a route with no valid rules", compiled.Listeners[0].AttachedRoutes)
	}
	if len(compiled.Routes[0].Rules) != 1 || len(compiled.Routes[0].Rules[0].Diagnostics) == 0 {
		t.Fatal("rejected route lost its rule diagnostics")
	}
}

func TestCompileRetainsMixedRuleDiagnostics(t *testing.T) {
	gateway := edgeGateway()
	route := edgeRoute(gateway)
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{unsupportedEdgeRule(), {}}
	compiled, err := Compile(Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{route}})
	if err != nil {
		t.Fatal(err)
	}
	parent := compiled.Routes[0].Parents[0]
	if parent.State != AttachmentAccepted || compiled.Listeners[0].AttachedRoutes != 1 {
		t.Fatalf("mixed route attachment = %s, count = %d, want accepted once", parent.State, compiled.Listeners[0].AttachedRoutes)
	}
	if len(compiled.Routes[0].Rules) != 2 || len(compiled.Routes[0].Rules[0].Diagnostics) == 0 || len(compiled.Routes[0].Rules[1].Diagnostics) != 0 {
		t.Fatal("mixed route did not preserve the distinction between unresolved and supported rules")
	}
	// Retain the more specific unresolved action so consumers cannot mistake a
	// partial compilation for permission to send its traffic to the fallback.
	matches := compiled.Listeners[0].Rules
	if len(matches) != 2 || matches[0].RuleIndex != 0 || matches[0].Match.PathValue != "/private" || matches[1].RuleIndex != 1 {
		t.Fatalf("ordered rules = %#v, want unresolved /private before the fallback", matches)
	}
}

func TestCompileListenerConflictsDoNotDependOnInputOrder(t *testing.T) {
	gateway := edgeGateway()
	gateway.Spec.Listeners = []gatewayv1.Listener{
		{Name: "first", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
		{Name: "second", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
		{Name: "third", Port: 80, Protocol: gatewayv1.HTTPSProtocolType},
	}
	first, err := Compile(Input{Gateway: gateway})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(gateway.Spec.Listeners)
	second, err := Compile(Input{Gateway: gateway})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("listener reordering changed diagnostics: before=%#v, after=%#v", first.Listeners, second.Listeners)
	}
	for _, listener := range first.Listeners {
		for i, diagnostic := range listener.Diagnostics {
			if slices.Contains(listener.Diagnostics[:i], diagnostic) {
				t.Fatalf("listener %s repeats diagnostic %#v", listener.Name, diagnostic)
			}
		}
	}
}

func TestCompileUnsupportedTCPDoesNotConflictWithHTTP(t *testing.T) {
	gateway := edgeGateway()
	gateway.Spec.Listeners = append(gateway.Spec.Listeners, gatewayv1.Listener{
		Name: "tcp", Port: 80, Protocol: gatewayv1.TCPProtocolType,
	})
	compiled, err := Compile(Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{edgeRoute(gateway)}})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Routes[0].Parents[0].State != AttachmentAccepted {
		t.Fatal("unsupported TCP listener prevented HTTP attachment on the same port")
	}
	for _, listener := range compiled.Listeners {
		switch listener.Name {
		case "http":
			if listener.AttachedRoutes != 1 || len(listener.Diagnostics) != 0 {
				t.Fatalf("HTTP listener = %#v, want one attached route without conflicts", listener)
			}
		case "tcp":
			if !slices.ContainsFunc(listener.Diagnostics, func(diagnostic Diagnostic) bool {
				return diagnostic.Reason == string(gatewayv1.ListenerReasonUnsupportedProtocol)
			}) {
				t.Fatal("TCP listener lost its unsupported protocol diagnostic")
			}
		}
	}
}

func edgeGateway() *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infrastructure", Name: "gateway", UID: "gateway-uid"},
		Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{
			{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
		}},
	}
}

func edgeRoute(gateway *gatewayv1.Gateway) gatewayv1.HTTPRoute {
	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: gateway.Namespace, Name: "route", UID: "route-uid"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Namespace: ptr.To(gatewayv1.Namespace(gateway.Namespace)), Name: gatewayv1.ObjectName(gateway.Name),
			}}},
			Rules: []gatewayv1.HTTPRouteRule{{}},
		},
	}
}

func unsupportedEdgeRule() gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Value: ptr.To("/private")}}},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
			ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{
				Name: "X-Example", Value: "value",
			}}},
		}},
	}
}
