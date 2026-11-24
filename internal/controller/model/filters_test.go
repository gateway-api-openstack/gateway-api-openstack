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
	"net/http"
	"reflect"
	"testing"

	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestCompileFiltersPreservesOrderAndHeaderOperations(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{Filters: []gatewayv1.HTTPRouteFilter{
		{
			Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Set: []gatewayv1.HTTPHeader{{Name: "X-B", Value: "original"}, {Name: "x-b", Value: "ignored"}, {Name: "X-A", Value: "a,b"}},
				Add: []gatewayv1.HTTPHeader{{Name: "X-Add", Value: "c,d"}}, Remove: []string{"X-Remove", "x-remove"},
			},
		},
		{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Hostname: ptr.To(gatewayv1.PreciseHostname("shop.example.com")), StatusCode: ptr.To(http.StatusMovedPermanently),
			},
		},
	}}
	before := rule.DeepCopy()
	filters, diagnostics := compileFilters(rule)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	wantHeaders := &RequestHeaders{
		Set: []Header{{Name: "x-a", Value: "a,b"}, {Name: "x-b", Value: "original"}},
		Add: []Header{{Name: "x-add", Value: "c,d"}}, Remove: []string{"x-remove"},
	}
	if len(filters) != 2 || filters[0].Type != gatewayv1.HTTPRouteFilterRequestHeaderModifier || !reflect.DeepEqual(filters[0].RequestHeaders, wantHeaders) || filters[1].Type != gatewayv1.HTTPRouteFilterRequestRedirect {
		t.Fatalf("filters = %#v", filters)
	}
	redirect := filters[1].Redirect
	if redirect.StatusCode != http.StatusMovedPermanently || redirect.Hostname == nil || *redirect.Hostname != "shop.example.com" {
		t.Fatalf("redirect = %#v", redirect)
	}
	filters[0].RequestHeaders.Set[0].Value = "changed"
	filters[0].RequestHeaders.Remove[0] = "changed"
	*redirect.Hostname = "changed.example.com"
	if !reflect.DeepEqual(rule, *before) {
		t.Fatal("compilation or model mutation changed the observed route")
	}

	rule.Filters[0], rule.Filters[1] = rule.Filters[1], rule.Filters[0]
	reversed, diagnostics := compileFilters(rule)
	if len(diagnostics) != 0 || reversed[0].Type != gatewayv1.HTTPRouteFilterRequestRedirect || reversed[1].Type != gatewayv1.HTTPRouteFilterRequestHeaderModifier {
		t.Fatalf("filter order was lost: filters=%#v, diagnostics=%#v", reversed, diagnostics)
	}
}

func TestCompileRedirectPreservesRequestFields(t *testing.T) {
	for _, code := range []*int{nil, ptr.To(http.StatusFound), ptr.To(http.StatusMovedPermanently)} {
		redirect, diagnostic := compileRedirect(&gatewayv1.HTTPRequestRedirectFilter{StatusCode: code})
		if diagnostic != nil {
			t.Fatal(diagnostic)
		}
		wantCode := http.StatusFound
		if code != nil {
			wantCode = *code
		}
		if redirect.StatusCode != wantCode || redirect.Hostname != nil || redirect.Scheme != nil || redirect.Port != nil || redirect.Path != nil {
			t.Fatalf("omitted fields gained replacements: %#v", redirect)
		}
	}
}

func TestCompileFiltersRejectsIncompatibleActions(t *testing.T) {
	header := gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{}}
	redirect := gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{}}
	for _, tc := range []struct {
		name string
		rule gatewayv1.HTTPRouteRule
	}{
		{name: "duplicate header filter", rule: gatewayv1.HTTPRouteRule{Filters: []gatewayv1.HTTPRouteFilter{header, header}}},
		{name: "duplicate redirect", rule: gatewayv1.HTTPRouteRule{Filters: []gatewayv1.HTTPRouteFilter{redirect, redirect}}},
		{name: "redirect with rewrite", rule: gatewayv1.HTTPRouteRule{Filters: []gatewayv1.HTTPRouteFilter{redirect, {Type: gatewayv1.HTTPRouteFilterURLRewrite}}}},
		{name: "redirect with backend", rule: gatewayv1.HTTPRouteRule{Filters: []gatewayv1.HTTPRouteFilter{redirect}, BackendRefs: []gatewayv1.HTTPBackendRef{{}}}},
		{name: "case equivalent add and set", rule: gatewayv1.HTTPRouteRule{Filters: []gatewayv1.HTTPRouteFilter{{
			Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "X-Value", Value: "set"}}, Add: []gatewayv1.HTTPHeader{{Name: "x-value", Value: "add"}}},
		}}}},
		{name: "case equivalent set and remove", rule: gatewayv1.HTTPRouteRule{Filters: []gatewayv1.HTTPRouteFilter{{
			Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "X-Value", Value: "set"}}, Remove: []string{"x-value"}},
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filters, diagnostics := compileFilters(tc.rule)
			if len(filters) != len(tc.rule.Filters) {
				t.Fatal("an invalid action disappeared from the rule")
			}
			for _, diagnostic := range diagnostics {
				if diagnostic.Reason == string(gatewayv1.RouteReasonIncompatibleFilters) && diagnostic.Scope == ScopeRule {
					return
				}
			}
			t.Fatalf("diagnostics = %#v, want rule-scoped IncompatibleFilters", diagnostics)
		})
	}
}

func TestCompileFiltersKeepsUnsupportedScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter gatewayv1.HTTPRouteFilter
		scope  DiagnosticScope
	}{
		{name: "missing header configuration", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier}, scope: ScopeRule},
		{name: "missing redirect configuration", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect}, scope: ScopeRule},
		{name: "known extended filter", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestMirror}, scope: ScopeRule},
		{name: "unknown filter", filter: gatewayv1.HTTPRouteFilter{Type: "FutureFilter"}, scope: ScopeRoute},
		{name: "extended 303 redirect", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{StatusCode: ptr.To(http.StatusSeeOther)}}, scope: ScopeRule},
		{name: "extended 307 redirect", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{StatusCode: ptr.To(http.StatusTemporaryRedirect)}}, scope: ScopeRule},
		{name: "extended 308 redirect", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{StatusCode: ptr.To(http.StatusPermanentRedirect)}}, scope: ScopeRule},
		{name: "unknown redirect code", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{StatusCode: ptr.To(399)}}, scope: ScopeRoute},
		{name: "extended redirect scheme", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Scheme: ptr.To("https")}}, scope: ScopeRule},
		{name: "unknown redirect scheme", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Scheme: ptr.To("future")}}, scope: ScopeRoute},
		{name: "extended redirect port", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Port: ptr.To(gatewayv1.PortNumber(8443))}}, scope: ScopeRule},
		{name: "extended redirect path", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Path: &gatewayv1.HTTPPathModifier{Type: gatewayv1.FullPathHTTPPathModifier, ReplaceFullPath: ptr.To("/replaced")}}}, scope: ScopeRule},
		{name: "unknown redirect path", filter: gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Path: &gatewayv1.HTTPPathModifier{Type: "FuturePath"}}}, scope: ScopeRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filters, diagnostics := compileFilters(gatewayv1.HTTPRouteRule{Filters: []gatewayv1.HTTPRouteFilter{tc.filter}})
			if len(filters) != 1 || filters[0].Type != tc.filter.Type || len(diagnostics) != 1 || diagnostics[0].Scope != tc.scope || diagnostics[0].Reason != string(gatewayv1.RouteReasonUnsupportedValue) {
				t.Fatalf("filters=%#v, diagnostics=%#v", filters, diagnostics)
			}
		})
	}
}

func TestCompilePreservesFiltersAndBackendFailureShares(t *testing.T) {
	route := logicalRoute("headers", "/")
	route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{
		Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Remove: []string{"X-Private"}},
	}}
	route.Spec.Rules[0].BackendRefs[0].Weight = ptr.To(int32(0))
	second := *route.Spec.Rules[0].BackendRefs[0].DeepCopy()
	second.Name, second.Weight = "missing", ptr.To(int32(7))
	route.Spec.Rules[0].BackendRefs = append(route.Spec.Rules[0].BackendRefs, second)
	// Header-only rules still retain an omitted backend list. The compiler
	// must not invent a forwarding target or drop their match from precedence.
	route.Spec.Rules = append(route.Spec.Rules, gatewayv1.HTTPRouteRule{Filters: route.Spec.Rules[0].Filters})
	got := compileLogical(t, Input{Gateway: logicalGateway(), Routes: []gatewayv1.HTTPRoute{route}})
	compiled := got.Routes[0]
	if len(compiled.Diagnostics) != 0 || compiled.Parents[0].State != AttachmentAccepted {
		t.Fatalf("Core filters prevented logical attachment: %#v", compiled)
	}
	rule := compiled.Rules[0]
	if len(rule.Diagnostics) != 0 || len(rule.Filters) != 1 || len(rule.Backends) != 2 || rule.Backends[0].Weight != 0 || rule.Backends[1].Weight != 7 || rule.Backends[1].State != BackendInvalid {
		t.Fatalf("filter compilation lost backend selection information: %#v", rule)
	}
	if len(compiled.Rules[1].Backends) != 0 || len(compiled.Rules[1].Filters) != 1 || len(got.Listeners[0].Rules) != 2 {
		t.Fatalf("omitted backend action disappeared: %#v", compiled.Rules[1])
	}
}

func TestCompileOrdersRedirectAndForwardingTogether(t *testing.T) {
	forward := logicalRoute("specific", "/api")
	redirect := logicalRoute("broad", "/")
	redirect.Spec.Rules[0].BackendRefs = nil
	redirect.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{
		Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{},
	}}
	got := compileLogical(t, Input{Gateway: logicalGateway(), Routes: []gatewayv1.HTTPRoute{redirect, forward}})
	if len(got.Listeners[0].Rules) != 2 || got.Listeners[0].Rules[0].Route.Name != "specific" || got.Listeners[0].Rules[1].Route.Name != "broad" {
		t.Fatalf("action type changed Gateway API precedence: %#v", got.Listeners[0].Rules)
	}
	compiled := got.Routes[0].Rules[0]
	if len(compiled.Diagnostics) != 0 || len(compiled.Backends) != 0 || len(compiled.Filters) != 1 || compiled.Filters[0].Redirect.StatusCode != http.StatusFound {
		t.Fatalf("redirect was interpreted as a missing backend: %#v", compiled)
	}
}
