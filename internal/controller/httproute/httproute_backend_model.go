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
	"fmt"
	"net"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/utils/ptr"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// backendNodes keeps readiness separate for each kube-proxy address family.
// An unnamed Node records a ready endpoint usable with Cluster traffic policy.
type backendNodes map[discoveryv1.AddressType]map[string]struct{}

// readyBackendNodes validates endpoints before the reader requests Nodes.
// Calculation is separate from I/O so endpoint and member costs can be measured.
func readyBackendNodes(service *corev1.Service, servicePort *corev1.ServicePort, slices []discoveryv1.EndpointSlice) (backendNodes, error) {
	readyEndpoint := false
	localNodes := backendNodes{}
	for _, slice := range slices {
		if !sliceSupportsServicePort(service, servicePort, slice) {
			continue
		}
		for _, endpoint := range slice.Endpoints {
			if !endpointReady(endpoint.Conditions) {
				continue
			}
			readyEndpoint = true
			nodeName := ptr.Deref(endpoint.NodeName, "")
			if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal && nodeName == "" {
				continue
			}
			if localNodes[slice.AddressType] == nil {
				localNodes[slice.AddressType] = map[string]struct{}{}
			}
			localNodes[slice.AddressType][nodeName] = struct{}{}
		}
	}
	if !readyEndpoint {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s has no ready endpoints", service.Name)
	}
	if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal && len(localNodes) == 0 {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s uses externalTrafficPolicy Local but ready endpoints do not identify their Nodes", service.Name)
	}

	return localNodes, nil
}

func sliceSupportsServicePort(service *corev1.Service, servicePort *corev1.ServicePort, slice discoveryv1.EndpointSlice) bool {
	if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
		return false
	}
	if len(service.Spec.IPFamilies) != 0 && !slices.Contains(service.Spec.IPFamilies, corev1.IPFamily(slice.AddressType)) {
		return false
	}
	protocol := servicePort.Protocol
	if protocol == "" {
		protocol = corev1.ProtocolTCP
	}
	for _, port := range slice.Ports {
		// EndpointSlice port names identify ServicePort names, not targetPort
		// names. A named targetPort may resolve to a different number per slice.
		if ptr.Deref(port.Name, "") == servicePort.Name && ptr.Deref(port.Protocol, corev1.ProtocolTCP) == protocol && ptr.Deref(port.Port, 0) > 0 {
			return true
		}
	}
	return false
}

func nodePortMembersForNodes(service *corev1.Service, nodePort int32, addressType corev1.NodeAddressType, localNodes backendNodes, nodes []corev1.Node) ([]cloud.Member, error) {
	members := make([]cloud.Member, 0, len(nodes))
	providersByAddress := map[string]string{}
	for index := range nodes {
		node := &nodes[index]
		if controller.NodePortNodeExcluded(node) || !readyNode(node) {
			continue
		}
		address := nodeAddress(node, addressType, service.Spec.ExternalTrafficPolicy, localNodes)
		if address == "" {
			continue
		}
		key := net.JoinHostPort(address, fmt.Sprintf("%d", nodePort))
		if providerID, duplicate := providersByAddress[key]; duplicate {
			if providerID != node.Spec.ProviderID {
				return nil, newRouteBuildError(routeErrorPending, "backend Service %s has worker Nodes with conflicting provider IDs for the same selected address", service.Name)
			}
			continue
		}
		providersByAddress[key] = node.Spec.ProviderID
		members = append(members, cloud.Member{Address: address, Port: int(nodePort), ProviderID: node.Spec.ProviderID})
	}
	if len(members) == 0 {
		return nil, newRouteBuildError(routeErrorPending, "backend Service %s has no eligible worker Nodes with a %s address", service.Name, addressType)
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Address == members[j].Address {
			return members[i].Port < members[j].Port
		}
		return members[i].Address < members[j].Address
	})
	return members, nil
}

func endpointReady(conditions discoveryv1.EndpointConditions) bool {
	if conditions.Terminating != nil && *conditions.Terminating {
		return false
	}
	return conditions.Ready == nil || *conditions.Ready
}

func readyNode(node *corev1.Node) bool {
	if !node.DeletionTimestamp.IsZero() || node.Spec.Unschedulable {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeAddress(node *corev1.Node, addressType corev1.NodeAddressType, policy corev1.ServiceExternalTrafficPolicy, readyNodes backendNodes) string {
	for _, address := range node.Status.Addresses {
		ip := net.ParseIP(address.Address)
		if address.Type != addressType || ip == nil {
			continue
		}
		family := discoveryv1.AddressTypeIPv6
		if ip.To4() != nil {
			family = discoveryv1.AddressTypeIPv4
		}
		if len(readyNodes[family]) == 0 {
			continue
		}
		if policy == corev1.ServiceExternalTrafficPolicyLocal {
			if _, ok := readyNodes[family][node.Name]; !ok {
				continue
			}
		}
		return address.Address
	}
	return ""
}
