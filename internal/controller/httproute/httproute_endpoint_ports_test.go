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
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func TestNodePortEndpointSlicePortSelection(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1.ServicePort, *discoveryv1.EndpointSlice)
		ready  bool
	}{
		{name: "named target resolves to endpoint port", ready: true},
		{name: "another named target resolution", ready: true, mutate: func(_ *corev1.ServicePort, slice *discoveryv1.EndpointSlice) {
			slice.Ports[0].Port = ptr.To(int32(9090))
		}},
		{name: "default TCP", ready: true, mutate: func(port *corev1.ServicePort, slice *discoveryv1.EndpointSlice) {
			port.Protocol = ""
			slice.Ports[0].Protocol = nil
		}},
		{name: "unnamed port", ready: true, mutate: func(port *corev1.ServicePort, slice *discoveryv1.EndpointSlice) {
			port.Name = ""
			slice.Ports[0].Name = nil
		}},
		{name: "different Service port", mutate: func(_ *corev1.ServicePort, slice *discoveryv1.EndpointSlice) {
			slice.Ports[0].Name = ptr.To("metrics")
		}},
		{name: "targetPort name is not Service port name", mutate: func(port *corev1.ServicePort, slice *discoveryv1.EndpointSlice) {
			slice.Ports[0].Name = ptr.To(port.TargetPort.StrVal)
		}},
		{name: "different protocol", mutate: func(_ *corev1.ServicePort, slice *discoveryv1.EndpointSlice) {
			slice.Ports[0].Protocol = ptr.To(corev1.ProtocolUDP)
		}},
		{name: "unresolved port", mutate: func(_ *corev1.ServicePort, slice *discoveryv1.EndpointSlice) {
			slice.Ports[0].Port = nil
		}},
		{name: "no ports", mutate: func(_ *corev1.ServicePort, slice *discoveryv1.EndpointSlice) { slice.Ports = nil }},
	} {
		for _, policy := range []corev1.ServiceExternalTrafficPolicy{corev1.ServiceExternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyLocal} {
			t.Run(test.name+"/"+string(policy), func(t *testing.T) {
				service, slice, node := validNodePortBackend("default", "backend")
				service.Spec.ExternalTrafficPolicy = policy
				service.Spec.Ports[0].Name = "http"
				service.Spec.Ports[0].TargetPort = intstr.FromString("application")
				slice.Ports = []discoveryv1.EndpointPort{{Name: ptr.To("http"), Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(int32(8080))}}
				if test.mutate != nil {
					test.mutate(&service.Spec.Ports[0], slice)
				}
				beforeService, beforeSlice, beforeNode := service.DeepCopy(), slice.DeepCopy(), node.DeepCopy()
				ready, err := readyBackendNodes(service, &service.Spec.Ports[0], []discoveryv1.EndpointSlice{*slice})
				if !test.ready {
					var pending *routeBuildError
					if !errors.As(err, &pending) || pending.kind != routeErrorPending {
						t.Fatalf("unusable Service port produced readiness: nodes=%v error=%v", ready, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					members, err := nodePortMembersForNodes(service, service.Spec.Ports[0].NodePort, corev1.NodeInternalIP, ready, []corev1.Node{*node})
					want := []cloud.Member{{Address: "192.0.2.10", Port: 30080}}
					if err != nil || !slices.Equal(members, want) {
						t.Fatalf("members=%v error=%v, want NodePort members %v", members, err, want)
					}
				}
				if !apiequality.Semantic.DeepEqual(service, beforeService) || !apiequality.Semantic.DeepEqual(slice, beforeSlice) || !apiequality.Semantic.DeepEqual(node, beforeNode) {
					t.Fatal("backend resolution changed its input objects")
				}
			})
		}
	}
}

func TestNodePortMembersKeepEndpointFamiliesSeparate(t *testing.T) {
	for _, test := range []struct {
		name     string
		policy   corev1.ServiceExternalTrafficPolicy
		families []corev1.IPFamily
		address  string
	}{
		{name: "Cluster can use remote IPv4 endpoint", policy: corev1.ServiceExternalTrafficPolicyCluster, families: []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, address: "192.0.2.10"},
		{name: "Local needs endpoint on same Node and family", policy: corev1.ServiceExternalTrafficPolicyLocal, families: []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, address: "2001:db8::10"},
		{name: "IPv4 Service cannot use local IPv6 slice", policy: corev1.ServiceExternalTrafficPolicyLocal, families: []corev1.IPFamily{corev1.IPv4Protocol}},
		{name: "IPv6 Service ignores remote IPv4 slice", policy: corev1.ServiceExternalTrafficPolicyCluster, families: []corev1.IPFamily{corev1.IPv6Protocol}, address: "2001:db8::10"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, ipv4, node := validNodePortBackend("default", "backend")
			service.Spec.ExternalTrafficPolicy, service.Spec.IPFamilies = test.policy, test.families
			ipv6 := ipv4.DeepCopy()
			ipv6.Name, ipv6.AddressType = "backend-ipv6", discoveryv1.AddressTypeIPv6
			ipv4.Endpoints[0].NodeName = ptr.To("another-worker")
			node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "2001:db8::10"})
			ready, err := readyBackendNodes(service, &service.Spec.Ports[0], []discoveryv1.EndpointSlice{*ipv4, *ipv6})
			if err != nil {
				t.Fatal(err)
			}
			members, err := nodePortMembersForNodes(service, 30080, corev1.NodeInternalIP, ready, []corev1.Node{*node})
			if test.address == "" {
				var pending *routeBuildError
				if !errors.As(err, &pending) || pending.kind != routeErrorPending || len(members) != 0 {
					t.Fatalf("mismatched address family produced members=%v error=%v", members, err)
				}
				return
			}
			if err != nil || len(members) != 1 || members[0].Address != test.address {
				t.Fatalf("members=%v error=%v, want member at %s", members, err, test.address)
			}
		})
	}
}

func TestUnrelatedEndpointSlicesCannotEstablishBackendReadiness(t *testing.T) {
	service, selected, node := validNodePortBackend("default", "backend")
	otherService, otherNamespace := selected.DeepCopy(), selected.DeepCopy()
	otherService.Name, otherService.Labels[discoveryv1.LabelServiceName] = "other-service", "other"
	otherNamespace.Namespace = "other"
	selected.Ports = nil
	reader := &fanoutCountingClient{Client: indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(selected, otherService, otherNamespace, node).Build()}
	members, err := nodePortMembersFromReader(t.Context(), reader, service, &service.Spec.Ports[0], corev1.NodeInternalIP)
	var pending *routeBuildError
	if !errors.As(err, &pending) || pending.kind != routeErrorPending || len(members) != 0 || reader.lists != 1 {
		t.Fatalf("unrelated slices established readiness or triggered Node reads: members=%v error=%v lists=%d", members, err, reader.lists)
	}
}
