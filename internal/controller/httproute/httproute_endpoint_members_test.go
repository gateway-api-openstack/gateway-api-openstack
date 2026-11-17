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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

func TestEndpointMembersConditionsAndConflictingObservations(t *testing.T) {
	ready := model.Endpoint{Address: "192.0.2.20", Port: 8080, AddressType: discoveryv1.AddressTypeIPv4, Ready: true, Serving: true}
	for _, test := range []struct {
		name      string
		change    func(*model.Endpoint)
		duplicate bool
		want      []cloud.Member
	}{
		{name: "ready", want: []cloud.Member{{Address: ready.Address, Port: 8080}}},
		{name: "duplicate", duplicate: true, want: []cloud.Member{{Address: ready.Address, Port: 8080}}},
		{name: "not ready", change: func(e *model.Endpoint) { e.Ready = false }},
		{name: "not serving", change: func(e *model.Endpoint) { e.Serving = false }},
		{name: "draining", change: func(e *model.Endpoint) { e.Ready, e.Terminating = false, true }, want: []cloud.Member{{Address: ready.Address, Port: 8080, Draining: true}}},
		{name: "terminated", change: func(e *model.Endpoint) { e.Terminating, e.Serving = true, false }},
		{name: "unavailable duplicate", duplicate: true, change: func(e *model.Endpoint) { e.Ready = false }},
		{name: "draining duplicate", duplicate: true, change: func(e *model.Endpoint) { e.Terminating = true }, want: []cloud.Member{{Address: ready.Address, Port: 8080, Draining: true}}},
		{name: "terminated duplicate", duplicate: true, change: func(e *model.Endpoint) { e.Terminating, e.Serving = true, false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint := ready
			if test.change != nil {
				test.change(&endpoint)
			}
			observations := []model.Endpoint{endpoint}
			if test.duplicate {
				observations = append(observations, ready)
			}
			for range 2 {
				members, err := endpointMembers(&corev1.Service{}, observations)
				if err != nil || !slices.Equal(members, test.want) || members == nil {
					t.Fatalf("members = %#v, error = %v, want %#v", members, err, test.want)
				}
				slices.Reverse(observations)
			}
		})
	}
}

func TestEndpointMembersRejectUnsafeAddresses(t *testing.T) {
	service := &corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: "192.0.2.100", ClusterIPs: []string{"192.0.2.100", "192.0.2.101"}}}
	for _, address := range []string{"127.0.0.1", "169.254.169.254", "224.0.0.1", "0.0.0.0", "255.255.255.255", "192.0.2.100", "192.0.2.101"} {
		t.Run(address, func(t *testing.T) {
			_, err := endpointMembers(service, []model.Endpoint{{Address: address, Port: 80, AddressType: discoveryv1.AddressTypeIPv4, Ready: true, Serving: true}})
			var rejected *routeBuildError
			if !errors.As(err, &rejected) || rejected.kind != routeErrorUnsupported {
				t.Fatalf("unsafe address accepted: %v", err)
			}
		})
	}
}

func endpointRouteFixture(t *testing.T) (*gatewayv1.GatewayClass, *gatewayv1.Gateway, *gatewayv1.HTTPRoute, *corev1.Service, *discoveryv1.EndpointSlice) {
	t.Helper()
	cfg := testConfig()
	class, gateway := providerRouteParentObjects(cfg)
	controller.ClearGatewayTopology(cfg, gateway)
	topology := testBoundTopology()
	topology.Topology.MemberMode = cloud.MemberModeEndpoint
	if err := controller.BindGatewayTopology(cfg, gateway, topology); err != nil {
		t.Fatal(err)
	}
	route := mapperTestHTTPRoute(gateway.Namespace, "api", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, "backend")
	route.UID, route.Generation = "route-uid", 1
	service, endpointSlice, _ := validNodePortBackend(gateway.Namespace, "backend")
	service.UID = "service-uid"
	service.Spec.Type = corev1.ServiceTypeClusterIP
	service.Spec.ClusterIP = "198.51.100.100"
	service.Spec.Ports[0].NodePort = 0
	service.Spec.Ports[0].Name = "http"
	service.Spec.Ports[0].TargetPort = intstr.FromString("container-http")
	endpointSlice.Ports[0].Name = ptr.To("http")
	endpointSlice.Ports[0].Port = ptr.To(int32(8080))
	endpointSlice.Endpoints[0].Addresses = []string{"192.0.2.20"}
	endpointSlice.Endpoints[0].NodeName = nil
	return class, gateway, route, service, endpointSlice
}

func TestEndpointRouteResolvesServiceInputsWithoutNodes(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*corev1.Service, *discoveryv1.EndpointSlice)
		want   []cloud.Member
	}{
		{name: "ClusterIP named target port", want: []cloud.Member{{Address: "192.0.2.20", Port: 8080}}},
		{name: "headless manual slice", change: func(s *corev1.Service, _ *discoveryv1.EndpointSlice) {
			s.Spec.ClusterIP = corev1.ClusterIPNone
			s.Spec.Selector = nil
		}, want: []cloud.Member{{Address: "192.0.2.20", Port: 8080}}},
		{name: "NodePort uses endpoint port", change: func(s *corev1.Service, _ *discoveryv1.EndpointSlice) {
			s.Spec.Type = corev1.ServiceTypeNodePort
			s.Spec.Ports[0].NodePort = 30080
			s.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		}, want: []cloud.Member{{Address: "192.0.2.20", Port: 8080}}},
		{name: "different named port", change: func(_ *corev1.Service, e *discoveryv1.EndpointSlice) { e.Ports[0].Name = ptr.To("container-http") }},
		{name: "no endpoints", change: func(_ *corev1.Service, e *discoveryv1.EndpointSlice) { e.Endpoints = nil }},
		{name: "nil conditions", change: func(_ *corev1.Service, e *discoveryv1.EndpointSlice) {
			e.Endpoints[0].Conditions = discoveryv1.EndpointConditions{}
		}, want: []cloud.Member{{Address: "192.0.2.20", Port: 8080}}},
		{name: "deleting slice", change: func(_ *corev1.Service, e *discoveryv1.EndpointSlice) {
			e.DeletionTimestamp = ptr.To(metav1.Now())
			e.Finalizers = []string{"example.test/hold"}
		}},
		{name: "dual stack uses IPv4", change: func(s *corev1.Service, _ *discoveryv1.EndpointSlice) {
			s.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}
		}, want: []cloud.Member{{Address: "192.0.2.20", Port: 8080}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, gateway, route, service, endpointSlice := endpointRouteFixture(t)
			if test.change != nil {
				test.change(service, endpointSlice)
			}
			ipv6 := endpointSlice.DeepCopy()
			ipv6.Name = "backend-ipv6"
			ipv6.AddressType = discoveryv1.AddressTypeIPv6
			ipv6.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"2001:db8::20"}}}
			reader := &fanoutCountingClient{Client: indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(service, endpointSlice, ipv6).Build()}
			r := &Reconciler{Config: testConfig()}
			desired, err := r.buildRouteModelWithReader(t.Context(), reader, route, gateway)
			if err != nil {
				t.Fatal(err)
			}
			if desired.MemberMode != cloud.MemberModeEndpoint || !slices.Equal(desired.Members, test.want) || desired.Members == nil {
				t.Fatalf("desired = %#v, want members %#v", desired, test.want)
			}
			if reader.gets != 1 || reader.lists != 1 {
				t.Fatalf("reads = %d Get, %d List, want only Service and EndpointSlices", reader.gets, reader.lists)
			}
		})
	}
}

func TestEndpointRouteKeepsPerSlicePortsAndStableMemberOrdering(t *testing.T) {
	_, gateway, route, service, first := endpointRouteFixture(t)
	second := first.DeepCopy()
	second.Name = "backend-other-port"
	second.Ports[0].Port = ptr.To(int32(9090))
	second.Endpoints[0].Addresses = []string{"192.0.2.10", "192.0.2.20"}
	reader := indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(service, second, first).Build()
	r := &Reconciler{Config: testConfig()}
	desired, err := r.buildRouteModelWithReader(t.Context(), reader, route, gateway)
	want := []cloud.Member{{Address: "192.0.2.10", Port: 9090}, {Address: "192.0.2.20", Port: 8080}, {Address: "192.0.2.20", Port: 9090}}
	if err != nil || !slices.Equal(desired.Members, want) {
		t.Fatalf("members = %#v, error = %v, want %#v", desired.Members, err, want)
	}
}

func TestEndpointRouteRejectsUnsupportedServiceInputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*corev1.Service)
	}{
		{name: "LoadBalancer", change: func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeLoadBalancer }},
		{name: "ExternalName", change: func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeExternalName }},
		{name: "IPv6 only", change: func(s *corev1.Service) { s.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol} }},
		{name: "UDP", change: func(s *corev1.Service) { s.Spec.Ports[0].Protocol = corev1.ProtocolUDP }},
		{name: "non HTTP appProtocol", change: func(s *corev1.Service) { s.Spec.Ports[0].AppProtocol = ptr.To("kubernetes.io/h2c") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, gateway, route, service, endpointSlice := endpointRouteFixture(t)
			test.change(service)
			reader := indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(service, endpointSlice).Build()
			r := &Reconciler{Config: testConfig()}
			decision, err := r.evaluateRouteSlotWithReader(t.Context(), reader, route, gateway)
			if err != nil || decision.canReserve || decision.rejection == nil {
				t.Fatalf("slot = %#v, error = %v", decision, err)
			}
			if _, err := r.buildRouteModelWithReader(t.Context(), reader, route, gateway); err == nil {
				t.Fatal("unsupported Service reached the cloud model")
			}
		})
	}
}

func TestNodeEventsDoNotEnqueueEndpointNodePortServices(t *testing.T) {
	r, reader, node := nodeFanoutFixture(t, 3, corev1.ServiceExternalTrafficPolicyLocal)
	var gateway gatewayv1.Gateway
	if err := reader.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "edge"}, &gateway); err != nil {
		t.Fatal(err)
	}
	controller.ClearGatewayTopology(r.Config, &gateway)
	topology := testBoundTopology()
	topology.Topology.MemberMode = cloud.MemberModeEndpoint
	if err := controller.BindGatewayTopology(r.Config, &gateway, topology); err != nil {
		t.Fatal(err)
	}
	if err := reader.Update(t.Context(), &gateway); err != nil {
		t.Fatal(err)
	}
	reader.gets, reader.lists = 0, 0
	if got := r.enqueueHTTPRoutesForNode(t.Context(), node); len(got) != 0 {
		t.Fatalf("Endpoint routes enqueued by Node: %v", got)
	}
	if reader.gets != 4 || reader.lists != 1 {
		t.Fatalf("Node mapper read Endpoint backend inputs: %d gets, %d lists", reader.gets, reader.lists)
	}
}
