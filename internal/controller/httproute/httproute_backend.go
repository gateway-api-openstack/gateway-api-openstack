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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func nodePortForBackend(service *corev1.Service, port int32) (*corev1.ServicePort, error) {
	for index := range service.Spec.Ports {
		candidate := &service.Spec.Ports[index]
		if candidate.Port != port {
			continue
		}
		if candidate.Protocol != "" && candidate.Protocol != corev1.ProtocolTCP {
			return nil, newRouteBuildError(routeErrorUnsupportedProtocol, "backend Service port %d must use TCP", port)
		}
		if candidate.AppProtocol != nil && *candidate.AppProtocol != "http" && *candidate.AppProtocol != "kubernetes.io/http" {
			return nil, newRouteBuildError(routeErrorUnsupportedProtocol, "backend Service appProtocol %q is unsupported", *candidate.AppProtocol)
		}
		if candidate.NodePort == 0 {
			return nil, newRouteBuildError(routeErrorUnsupported, "backend Service port %d has no allocated NodePort", port)
		}
		return candidate, nil
	}
	return nil, newRouteBuildError(routeErrorPending, "backend Service %s does not expose port %d", service.Name, port)
}

func nodePortMembersFromReader(ctx context.Context, reader client.Reader, service *corev1.Service, nodePort int32, addressType corev1.NodeAddressType) ([]cloud.Member, error) {
	var slices discoveryv1.EndpointSliceList
	if err := reader.List(
		ctx,
		&slices,
		client.InNamespace(service.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: service.Name},
	); err != nil {
		return nil, fmt.Errorf("list backend EndpointSlices: %w", err)
	}
	localNodes, err := readyBackendNodes(service, slices.Items)
	if err != nil {
		return nil, err
	}
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list backend Nodes: %w", err)
	}
	return nodePortMembersForNodes(service, nodePort, addressType, localNodes, nodes.Items)
}
