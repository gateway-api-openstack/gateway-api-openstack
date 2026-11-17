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
	if desired.Hostname != "api.example.test" || desired.PathType != cloud.PathMatchPrefix || desired.PathValue != "/api" {
		t.Fatalf("cloud match did not use normalized logical model: %#v", desired)
	}
	want := []cloud.Member{{Address: "192.0.2.10", Port: 30080}}
	if !slices.Equal(desired.Members, want) {
		t.Fatalf("members = %v, want existing NodePort transport %v", desired.Members, want)
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
		{name: "multiple rules", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules = append(route.Spec.Rules, *route.Spec.Rules[0].DeepCopy())
		}},
		{name: "unresolved filter", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterExtensionRef}}
		}},
		{name: "unresolved backend", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules[0].BackendRefs[0].Name = "missing"
		}},
		{name: "header predicate", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Rules[0].Matches = []gatewayv1.HTTPRouteMatch{{Headers: []gatewayv1.HTTPHeaderMatch{{Name: "x-version", Value: "v2"}}}}
		}},
		{name: "wildcard hostname", change: func(route *gatewayv1.HTTPRoute) {
			route.Spec.Hostnames = []gatewayv1.Hostname{"*.example.test"}
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
			_, _, err = supportedRouteModel(logical, cloud.MemberModeNodePort)
			var rejected *routeBuildError
			if !errors.As(err, &rejected) || rejected.kind != routeErrorUnsupported {
				t.Fatalf("unmapped logical model escaped NodePort lowering: %v", err)
			}
		})
	}
}
