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
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

func TestNodePortRouteUsesLogicalMatchAndSharedEndpointObservation(t *testing.T) {
	cfg := testConfig()
	_, gateway := providerRouteParentObjects(cfg)
	route := mapperTestHTTPRoute(gateway.Namespace, "api", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, "backend")
	route.UID = "route-uid"
	route.Spec.Hostnames = []gatewayv1.Hostname{"api.example.test"}
	route.Spec.Rules[0].Matches = []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{
		Type: ptr.To(gatewayv1.PathMatchPathPrefix), Value: ptr.To("/api/"),
	}}}
	service, endpoints, node := validNodePortBackend(gateway.Namespace, "backend")
	endpoints.Endpoints[0].Addresses = []string{"198.51.100.20"}
	reader := &fanoutCountingClient{Client: indexedFakeClientBuilder(testScheme(t), cfg).WithObjects(service, endpoints, node).Build()}
	r := &Reconciler{Config: cfg}
	desired, err := r.buildRouteModelWithReader(t.Context(), reader, route, gateway)
	if err != nil {
		t.Fatal(err)
	}
	if desired.Rules[0].Matches[0].Hostname != "api.example.test" || desired.Rules[0].Matches[0].PathType != cloud.PathMatchPrefix || desired.Rules[0].Matches[0].PathValue != "/api" {
		t.Fatalf("cloud match did not use normalized logical model: %#v", desired)
	}
	want := []cloud.Member{{Address: "192.0.2.10", Port: 30080}}
	if !slices.Equal(desired.Rules[0].Members, want) {
		t.Fatalf("members = %v, want existing NodePort transport %v", desired.Rules[0].Members, want)
	}
	if reader.gets != 1 || reader.lists != 2 {
		t.Fatalf("reads = %d gets, %d lists, want one Service, EndpointSlice list, and Node list", reader.gets, reader.lists)
	}
}

func TestLogicalModelCannotEnableUnmappedNodePortBehavior(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*gatewayv1.HTTPRoute)
	}{
		{name: "multiple backends", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules[0].BackendRefs = append(route.Spec.Rules[0].BackendRefs, route.Spec.Rules[0].BackendRefs[0])
		}},
		{name: "unresolved filter", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterExtensionRef}}
		}},
		{name: "modeled request headers", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{
					Name: "x-version", Value: "v2",
				}}},
			}}
		}},
		{name: "modeled redirect", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules[0].BackendRefs = nil
			route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{
				Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Hostname: ptr.To(gatewayv1.PreciseHostname("redirect.example.test"))},
			}}
		}},
		{name: "unresolved backend", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules[0].BackendRefs[0].Name = "missing"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, gateway := providerRouteParentObjects(testConfig())
			route := mapperTestHTTPRoute(gateway.Namespace, "api", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, "backend")
			route.UID = "route-uid"
			service, endpoints, _ := validNodePortBackend(gateway.Namespace, "backend")
			test.change(route)
			logical, err := model.Compile(model.Input{
				Gateway: gateway, Routes: []gatewayv1.HTTPRoute{*route},
				Services: []corev1.Service{*service}, EndpointSlices: []discoveryv1.EndpointSlice{*endpoints},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = model.LowerHTTPForwarding(logical)
			var rejected *model.Diagnostic
			if !errors.As(err, &rejected) {
				t.Fatalf("unmapped logical model escaped NodePort lowering: %v", err)
			}
		})
	}
}

func TestRuleOptionsAreRejectedBeforeBackendReads(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*gatewayv1.HTTPRouteRule)
	}{
		{name: "name", change: func(rule *gatewayv1.HTTPRouteRule) { rule.Name = ptr.To(gatewayv1.SectionName("named")) }},
		{name: "timeouts", change: func(rule *gatewayv1.HTTPRouteRule) { rule.Timeouts = &gatewayv1.HTTPRouteTimeouts{} }},
		{name: "retry", change: func(rule *gatewayv1.HTTPRouteRule) { rule.Retry = &gatewayv1.HTTPRouteRetry{} }},
		{name: "session persistence", change: func(rule *gatewayv1.HTTPRouteRule) { rule.SessionPersistence = &gatewayv1.SessionPersistence{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, gateway := providerRouteParentObjects(testConfig())
			route := mapperTestHTTPRoute(gateway.Namespace, "route", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, "backend")
			test.change(&route.Spec.Rules[0])
			reader := &fanoutCountingClient{Client: indexedFakeClientBuilder(testScheme(t), testConfig()).Build()}
			r := &Reconciler{Config: testConfig()}
			_, err := r.buildRouteModelWithReader(t.Context(), reader, route, gateway)
			var rejected *routeBuildError
			if !errors.As(err, &rejected) || rejected.kind != routeErrorUnsupported || rejected.message != "named rules, timeouts, retries, and session persistence are not supported" {
				t.Fatalf("runtime rule option diagnostic = %v", err)
			}
			if reader.gets != 0 || reader.lists != 0 {
				t.Fatalf("unsupported rule caused %d Get and %d List calls", reader.gets, reader.lists)
			}
			if _, err := r.buildRouteModelFromInputs(t.Context(), reader, route, gateway, nil); !errors.As(err, &rejected) || rejected.kind != routeErrorUnsupported {
				t.Fatalf("existing-input entry point skipped structural validation: %v", err)
			}
			logical, err := model.Compile(model.Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{*route}})
			if err != nil {
				t.Fatal(err)
			}
			diagnostic := logical.Routes[0].Rules[0].Diagnostics[0]
			if diagnostic.Reason != string(gatewayv1.RouteReasonUnsupportedValue) || diagnostic.Scope != model.ScopeRule || diagnostic.Message != "named rules, timeouts, retries, and session persistence are not implemented" {
				t.Fatalf("logical rule option diagnostic = %#v", diagnostic)
			}
		})
	}
}

func TestHTTPMappingRejectsLogicalTLSListener(t *testing.T) {
	_, gateway := providerRouteParentObjects(testConfig())
	route := mapperTestHTTPRoute(gateway.Namespace, "api", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, "backend")
	service, endpoints, _ := validNodePortBackend(gateway.Namespace, "backend")
	logical, err := model.Compile(model.Input{
		Gateway: gateway, Routes: []gatewayv1.HTTPRoute{*route},
		Services: []corev1.Service{*service}, EndpointSlices: []discoveryv1.EndpointSlice{*endpoints},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.LowerHTTPForwarding(logical); err != nil {
		t.Fatalf("HTTP baseline: %v", err)
	}
	// A resolved certificate is logical input, not a deployed TLS capability.
	logical.Listeners[0].Protocol = gatewayv1.HTTPSProtocolType
	logical.Listeners[0].TLS = &model.ListenerTLS{}
	_, err = model.LowerHTTPForwarding(logical)
	var rejected *model.Diagnostic
	if !errors.As(err, &rejected) {
		t.Fatalf("TLS listener escaped HTTP lowering: %v", err)
	}
}

func TestRouteForwardingMappingUsesSeparateRulePoolsAndSharedNodeRead(t *testing.T) {
	cfg := testConfig()
	_, gateway := providerRouteParentObjects(cfg)
	route := mapperTestHTTPRoute(gateway.Namespace, "api", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, "backend")
	route.UID = "route-uid"
	route.Spec.Hostnames = []gatewayv1.Hostname{"*.example.test"}
	route.Spec.Rules[0].Matches = []gatewayv1.HTTPRouteMatch{{
		Path:    &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchExact), Value: ptr.To("/api")},
		Headers: []gatewayv1.HTTPHeaderMatch{{Name: "X-Version", Value: "v2"}},
	}}
	route.Spec.Rules = append(route.Spec.Rules, *route.Spec.Rules[0].DeepCopy())
	route.Spec.Rules[1].Matches[0].Path.Value = ptr.To("/other")
	service, endpoints, node := validNodePortBackend(gateway.Namespace, "backend")
	reader := &fanoutCountingClient{Client: indexedFakeClientBuilder(testScheme(t), cfg).WithObjects(service, endpoints, node).Build()}
	r := &Reconciler{Config: cfg}
	desired, err := r.buildRouteModelWithReader(t.Context(), reader, route, gateway)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired.Rules) != 2 || desired.Rules[0].Index != 0 || desired.Rules[1].Index != 1 {
		t.Fatalf("rule identities = %#v", desired.Rules)
	}
	for _, rule := range desired.Rules {
		if len(rule.Matches) != 1 || rule.Matches[0].Hostname != "*.example.test" || !slices.Equal(rule.Matches[0].Headers, []cloud.HeaderMatch{{Name: "x-version", Value: "v2"}}) {
			t.Fatalf("rule predicate = %#v", rule)
		}
	}
	if reader.gets != 1 || reader.lists != 2 {
		t.Fatalf("reads = %d gets, %d lists, want shared backend and Node observations", reader.gets, reader.lists)
	}
	if structurallySupportedRoute(route) {
		t.Fatal("physical mapping enabled the unsupported multi-rule failure lifecycle")
	}
}
