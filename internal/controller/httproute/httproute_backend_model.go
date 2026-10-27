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
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// readyBackendNodes validates endpoints before the reader requests Nodes.
// Calculation is separate from I/O so endpoint and member costs can be measured.
func readyBackendNodes(service *corev1.Service, slices []discoveryv1.EndpointSlice) (map[string]struct{}, error) {
	readyEndpoint := false
	localNodes := map[string]struct{}{}
	for _, slice := range slices {
		for _, endpoint := range slice.Endpoints {
			if !endpointReady(endpoint.Conditions) {
				continue
			}
			readyEndpoint = true
			if endpoint.NodeName != nil {
				localNodes[*endpoint.NodeName] = struct{}{}
			}
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

func nodePortMembersForNodes(service *corev1.Service, nodePort int32, addressType corev1.NodeAddressType, localNodes map[string]struct{}, nodes []corev1.Node) ([]cloud.Member, error) {
	members := make([]cloud.Member, 0, len(nodes))
	providersByAddress := map[string]string{}
	for index := range nodes {
		node := &nodes[index]
		if controller.NodePortNodeExcluded(node) || !readyNode(node) {
			continue
		}
		if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
			if _, ok := localNodes[node.Name]; !ok {
				continue
			}
		}
		address := nodeAddress(node, addressType)
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

func nodeAddress(node *corev1.Node, addressType corev1.NodeAddressType) string {
	for _, address := range node.Status.Addresses {
		if address.Type == addressType && net.ParseIP(address.Address) != nil {
			return address.Address
		}
	}
	return ""
}
