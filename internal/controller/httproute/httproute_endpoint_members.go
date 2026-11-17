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
	"cmp"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

type endpointAvailability uint8

const (
	endpointUnavailable endpointAvailability = iota
	endpointDraining
	endpointReadyForTraffic
)

// endpointMembers resolves conflicting observations conservatively. Removing an
// unavailable duplicate must be observed before another copy can restore traffic.
func endpointMembers(service *corev1.Service, endpoints []model.Endpoint) ([]cloud.Member, error) {
	type endpointKey struct {
		address string
		port    int32
	}
	selected := make(map[endpointKey]endpointAvailability)
	for _, endpoint := range endpoints {
		if endpoint.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		address, err := netip.ParseAddr(endpoint.Address)
		if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsLoopback() ||
			slices.Contains(service.Spec.ClusterIPs, address.String()) || service.Spec.ClusterIP == address.String() {
			return nil, newRouteBuildError(routeErrorUnsupported, "backend EndpointSlice contains an unsafe endpoint address")
		}
		key := endpointKey{address: address.String(), port: endpoint.Port}
		state := endpointUnavailable
		if endpoint.Serving {
			if endpoint.Terminating {
				state = endpointDraining
			} else if endpoint.Ready {
				state = endpointReadyForTraffic
			}
		}
		if previous, found := selected[key]; !found || state < previous {
			selected[key] = state
		}
	}
	members := make([]cloud.Member, 0, len(selected))
	for endpoint, state := range selected {
		if state == endpointUnavailable {
			continue
		}
		members = append(members, cloud.Member{Address: endpoint.address, Port: int(endpoint.port), Draining: state == endpointDraining})
	}
	slices.SortFunc(members, func(left, right cloud.Member) int {
		return cmp.Or(cmp.Compare(left.Address, right.Address), cmp.Compare(left.Port, right.Port))
	})
	return members, nil
}
