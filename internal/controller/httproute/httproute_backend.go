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
	"slices"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

func servicePortForBackend(service *corev1.Service, port gatewayv1.PortNumber, mode cloud.MemberMode) (*corev1.ServicePort, error) {
	if !service.DeletionTimestamp.IsZero() {
		return nil, newRouteBuildError(routeErrorPending, "backend Service is being deleted")
	}
	switch mode {
	case cloud.MemberModeNodePort:
		if service.Spec.Type != corev1.ServiceTypeNodePort {
			return nil, newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type NodePort", service.Name)
		}
	case cloud.MemberModeEndpoint:
		if len(service.Spec.IPFamilies) != 0 && !slices.Contains(service.Spec.IPFamilies, corev1.IPv4Protocol) {
			return nil, newRouteBuildError(routeErrorUnsupported, "endpoint members require an IPv4 Service")
		}
		if service.Spec.Type != "" && service.Spec.Type != corev1.ServiceTypeClusterIP && service.Spec.Type != corev1.ServiceTypeNodePort {
			return nil, newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type ClusterIP or NodePort", service.Name)
		}
	default:
		return nil, newRouteBuildError(routeErrorUnsupported, "backend member mode is unsupported")
	}
	selected, diagnostic := model.ResolveServicePort(service, &port)
	if diagnostic != nil {
		kind := routeErrorPending
		if diagnostic.Reason == string(gatewayv1.RouteReasonUnsupportedProtocol) {
			kind = routeErrorUnsupportedProtocol
		}
		return nil, newRouteBuildError(kind, "%s", diagnostic.Message)
	}
	if mode == cloud.MemberModeNodePort && selected.NodePort == 0 {
		return nil, newRouteBuildError(routeErrorUnsupported, "backend Service port %d has no allocated NodePort", port)
	}
	return selected, nil
}

func nodePortMembersFromReader(ctx context.Context, reader client.Reader, service *corev1.Service, servicePort *corev1.ServicePort, addressType corev1.NodeAddressType) ([]cloud.Member, error) {
	slices, err := backendEndpointSlices(ctx, reader, service)
	if err != nil {
		return nil, err
	}
	return nodePortMembersFromSlices(ctx, reader, service, servicePort, addressType, slices)
}

func backendEndpointSlices(ctx context.Context, reader client.Reader, service *corev1.Service) ([]discoveryv1.EndpointSlice, error) {
	var slices discoveryv1.EndpointSliceList
	if err := reader.List(
		ctx,
		&slices,
		client.InNamespace(service.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: service.Name},
	); err != nil {
		return nil, fmt.Errorf("list backend EndpointSlices: %w", err)
	}
	return slices.Items, nil
}

// nodePortMembersFromSlices uses the same endpoint observation as the logical
// model while retaining the existing NodePort transport and worker selection.
func nodePortMembersFromSlices(ctx context.Context, reader client.Reader, service *corev1.Service, servicePort *corev1.ServicePort, addressType corev1.NodeAddressType, slices []discoveryv1.EndpointSlice) ([]cloud.Member, error) {
	localNodes, err := readyBackendNodes(service, servicePort, slices)
	if err != nil {
		return nil, err
	}
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list backend Nodes: %w", err)
	}
	return nodePortMembersForNodes(service, servicePort.NodePort, addressType, localNodes, nodes.Items)
}
