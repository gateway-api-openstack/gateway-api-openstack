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
	"reflect"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func logicalGateway() *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "shop", UID: "gateway-uid", Generation: 2, ResourceVersion: "3"},
		Spec:       gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}}},
	}
}

func logicalRoute(name, path string) gatewayv1.HTTPRoute {
	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "apps", Name: name, UID: types.UID(name + "-uid"), Generation: 4, ResourceVersion: "5",
			CreationTimestamp: metav1.NewTime(time.Unix(100, 0)),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "shop"}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Value: &path}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "backend", Port: ptr.To(gatewayv1.PortNumber(80)),
				}}}},
			}},
		},
	}
}

func compileLogical(t *testing.T, input Input) Gateway {
	t.Helper()
	result, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCompileOrdersWholeGateway(t *testing.T) {
	broad := logicalRoute("broad", "/api")
	specific := logicalRoute("specific", "/api/admin/")
	exact := logicalRoute("exact", "/api/admin")
	exact.Spec.Rules[0].Matches[0].Path.Type = ptr.To(gatewayv1.PathMatchExact)
	header := *specific.DeepCopy()
	header.Name, header.UID = "header", "header-uid"
	header.Spec.Rules[0].Matches[0].Headers = []gatewayv1.HTTPHeaderMatch{{Name: "X-Tenant", Value: "test"}, {Name: "x-tenant", Value: "ignored"}}
	input := Input{Gateway: logicalGateway(), Routes: []gatewayv1.HTTPRoute{broad, specific, exact, header}}
	got := compileLogical(t, input)
	names := make([]string, 0)
	for _, match := range got.Listeners[0].Rules {
		names = append(names, match.Route.Name)
	}
	if want := []string{"exact", "header", "specific", "broad"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("route order = %v, want %v", names, want)
	}
	if got.Listeners[0].AttachedRoutes != 4 || got.Listeners[0].Rules[1].Match.PathValue != "/api/admin" {
		t.Fatalf("listener = %#v", got.Listeners[0])
	}
	slices.Reverse(input.Routes)
	if reversed := compileLogical(t, input); !reflect.DeepEqual(got, reversed) {
		t.Fatal("route input order changed the model")
	}
}

func TestCompileHostnamePrecedesPathAndRouteAge(t *testing.T) {
	wildcard := logicalRoute("wildcard", "/longer")
	wildcard.Spec.Hostnames = []gatewayv1.Hostname{"*.example.com"}
	exact := logicalRoute("exact", "/")
	exact.Spec.Hostnames = []gatewayv1.Hostname{"shop.example.com"}
	exact.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))
	got := compileLogical(t, Input{Gateway: logicalGateway(), Routes: []gatewayv1.HTTPRoute{wildcard, exact}})
	if got.Listeners[0].Rules[0].Route.Name != "exact" {
		t.Fatalf("hostname did not take precedence: %#v", got.Listeners[0].Rules)
	}
	older := logicalRoute("z-older", "/")
	newer := logicalRoute("a-newer", "/")
	newer.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))
	newer.Spec.Rules = append(newer.Spec.Rules, *newer.Spec.Rules[0].DeepCopy())
	got = compileLogical(t, Input{Gateway: logicalGateway(), Routes: []gatewayv1.HTTPRoute{newer, older}})
	if rules := got.Listeners[0].Rules; rules[0].Route.Name != older.Name || rules[1].RuleIndex != 0 || rules[2].RuleIndex != 1 {
		t.Fatalf("route age and rule order = %#v", rules)
	}
}

func TestCompileScopesParentsAndRetainsProvenance(t *testing.T) {
	route := logicalRoute("multi-parent", "/")
	route.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "another"}, {Name: "shop", SectionName: ptr.To(gatewayv1.SectionName("http"))}}
	unrelated := logicalRoute("unrelated", "/")
	unrelated.Spec.ParentRefs[0].Group = ptr.To(gatewayv1.Group(""))
	got := compileLogical(t, Input{Gateway: logicalGateway(), Routes: []gatewayv1.HTTPRoute{route, unrelated}})
	if len(got.Routes) != 1 || len(got.Routes[0].Parents) != 1 || got.Routes[0].Parents[0].Index != 1 || got.Routes[0].Parents[0].State != AttachmentAccepted {
		t.Fatalf("scoped route = %#v", got.Routes)
	}
	if source := got.Listeners[0].Rules[0].Route; source.UID != route.UID || source.Generation != route.Generation || source.ResourceVersion != route.ResourceVersion {
		t.Fatalf("source = %#v", source)
	}
	second := logicalGateway()
	second.Name = "another"
	second.UID = "second-gateway"
	other := compileLogical(t, Input{Gateway: second, Routes: []gatewayv1.HTTPRoute{route}})
	if other.Routes[0].Parents[0].Index != 0 || other.Source.UID == got.Source.UID {
		t.Fatalf("second Gateway reused another parent: %#v", other)
	}
}

func TestCompileAttachmentNamespacesHostnamesAndPorts(t *testing.T) {
	gateway := logicalGateway()
	gateway.Spec.Listeners[0].Hostname = ptr.To(gatewayv1.Hostname("*.example.com"))
	gateway.Spec.Listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
		From: ptr.To(gatewayv1.NamespacesFromSelector), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "shop"}},
	}}
	route := logicalRoute("cross-namespace", "/")
	route.Namespace = "tenant"
	route.Spec.ParentRefs[0].Namespace = ptr.To(gatewayv1.Namespace("apps"))
	route.Spec.ParentRefs[0].Port = ptr.To(gatewayv1.PortNumber(80))
	route.Spec.ParentRefs[0].SectionName = ptr.To(gatewayv1.SectionName("http"))
	route.Spec.Hostnames = []gatewayv1.Hostname{"example.com", "shop.example.com", "other.invalid", "*.sub.example.com"}
	input := Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{route}, Namespaces: []corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: map[string]string{"team": "shop"}}}}}
	got := compileLogical(t, input)
	parent := got.Routes[0].Parents[0]
	if parent.State != AttachmentAccepted || !reflect.DeepEqual(parent.Listeners[0].Hostnames, []string{"*.sub.example.com", "shop.example.com"}) {
		t.Fatalf("attachment = %#v", parent)
	}
	input.Routes[0].Spec.ParentRefs[0].Port = ptr.To(gatewayv1.PortNumber(8080))
	parent = compileLogical(t, input).Routes[0].Parents[0]
	if parent.State != AttachmentRejected || parent.Diagnostics[0].Reason != string(gatewayv1.RouteReasonNoMatchingParent) {
		t.Fatalf("section and port were not both enforced: %#v", parent)
	}
	input.Routes[0].Spec.ParentRefs[0].Port = ptr.To(gatewayv1.PortNumber(80))
	input.Namespaces[0].Labels["team"] = "different"
	parent = compileLogical(t, input).Routes[0].Parents[0]
	if parent.State != AttachmentRejected || parent.Diagnostics[0].Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("namespace selector was not enforced: %#v", parent)
	}
}

func TestCompileDoesNotMutateInputsOrOtherRoutes(t *testing.T) {
	gateway := logicalGateway()
	gateway.Spec.Listeners = append(gateway.Spec.Listeners, gatewayv1.Listener{Name: "second", Port: 8080, Protocol: gatewayv1.HTTPProtocolType})
	first := logicalRoute("first", "/api")
	first.Spec.Rules[0].Matches[0].Headers = []gatewayv1.HTTPHeaderMatch{{Name: "X-B", Value: "b"}, {Name: "X-A", Value: "a"}}
	second := logicalRoute("second", "/other")
	input := Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{second, first}}
	before, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	got := compileLogical(t, input)
	after, err := json.Marshal(input)
	if err != nil || string(before) != string(after) {
		t.Fatalf("compiler changed inputs: %v", err)
	}
	slices.Reverse(input.Gateway.Spec.Listeners)
	if reversed := compileLogical(t, input); !reflect.DeepEqual(got, reversed) {
		t.Fatal("listener order changed the logical model")
	}
	input.Routes = []gatewayv1.HTTPRoute{first}
	remaining := compileLogical(t, input)
	if !reflect.DeepEqual(got.Routes[0], remaining.Routes[0]) {
		t.Fatal("removing an unrelated route changed the remaining logical route")
	}
	got.Routes[0].Parents[0].Reference.Name = "changed"
	got.Routes[0].Rules[0].Matches[0].Headers[0].Value = "changed"
	if first.Spec.ParentRefs[0].Name != "shop" || first.Spec.Rules[0].Matches[0].Headers[1].Value != "a" {
		t.Fatal("output aliases input fields")
	}
}

func TestCompileRejectsAmbiguousInput(t *testing.T) {
	if _, err := Compile(Input{}); err == nil {
		t.Fatal("missing Gateway accepted")
	}
	route := logicalRoute("duplicate", "/")
	if _, err := Compile(Input{Gateway: logicalGateway(), Routes: []gatewayv1.HTTPRoute{route, route}}); err == nil {
		t.Fatal("duplicate HTTPRoute accepted")
	}
}

func TestCompileHTTPSRequiresCertificateInputs(t *testing.T) {
	gateway := logicalGateway()
	gateway.Spec.Listeners = append(gateway.Spec.Listeners, gatewayv1.Listener{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType})
	got := compileLogical(t, Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{logicalRoute("backend", "/")}})
	if got.Listeners[0].Name != "http" || got.Listeners[0].AttachedRoutes != 1 || len(got.Listeners[0].Rules) != 1 {
		t.Fatalf("HTTP listener was lost: %#v", got.Listeners)
	}
	if len(got.Listeners[1].Diagnostics) != 1 || got.Listeners[1].AttachedRoutes != 0 {
		t.Fatalf("HTTPS listener claimed resolution: %#v", got.Listeners[1])
	}
}

func TestNormalizeMatch(t *testing.T) {
	for _, test := range []struct {
		name  string
		input gatewayv1.HTTPRouteMatch
		path  string
		scope DiagnosticScope
	}{
		{name: "default", path: "/"},
		{name: "prefix trailing slash", input: gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Value: ptr.To("/api/")}}, path: "/api"},
		{name: "exact trailing slash", input: gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchExact), Value: ptr.To("/api/")}}, path: "/api/"},
		{name: "invalid path", input: gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Value: ptr.To("/api//")}}, scope: ScopeRule},
		{name: "encoded slash", input: gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Value: ptr.To("/api%2Fother")}}, scope: ScopeRule},
		{name: "unknown path type", input: gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchType("unknown"))}}, scope: ScopeRoute},
		{name: "known method", input: gatewayv1.HTTPRouteMatch{Method: ptr.To(gatewayv1.HTTPMethodGet)}, scope: ScopeRule},
		{name: "unknown method", input: gatewayv1.HTTPRouteMatch{Method: ptr.To(gatewayv1.HTTPMethod("unknown"))}, scope: ScopeRoute},
	} {
		t.Run(test.name, func(t *testing.T) {
			match, err := NormalizeMatch(test.input)
			if test.scope != "" {
				diagnostic, ok := err.(*Diagnostic)
				if !ok || diagnostic.Scope != test.scope {
					t.Fatalf("error = %v, want scope %s", err, test.scope)
				}
				return
			}
			if err != nil || match.PathValue != test.path {
				t.Fatalf("match = %#v, error = %v", match, err)
			}
		})
	}
}
