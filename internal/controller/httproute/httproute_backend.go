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
	"slices"

	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

func validateBackendServicePort(service *corev1.Service, port gatewayv1.PortNumber, mode cloud.MemberMode) error {
	if !service.DeletionTimestamp.IsZero() {
		return newRouteBuildError(routeErrorPending, "backend Service is being deleted")
	}
	switch mode {
	case cloud.MemberModeNodePort:
		if service.Spec.Type != corev1.ServiceTypeNodePort {
			return newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type NodePort", service.Name)
		}
	case cloud.MemberModeEndpoint:
		if len(service.Spec.IPFamilies) != 0 && !slices.Contains(service.Spec.IPFamilies, corev1.IPv4Protocol) {
			return newRouteBuildError(routeErrorUnsupported, "endpoint members require an IPv4 Service")
		}
		if service.Spec.Type != "" && service.Spec.Type != corev1.ServiceTypeClusterIP && service.Spec.Type != corev1.ServiceTypeNodePort {
			return newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type ClusterIP or NodePort", service.Name)
		}
	default:
		return newRouteBuildError(routeErrorUnsupported, "backend member mode is unsupported")
	}
	selected, diagnostic := model.ResolveServicePort(service, &port)
	if diagnostic != nil {
		kind := routeErrorPending
		if diagnostic.Reason == string(gatewayv1.RouteReasonUnsupportedProtocol) {
			kind = routeErrorUnsupportedProtocol
		}
		return newRouteBuildError(kind, "%s", diagnostic.Message)
	}
	if mode == cloud.MemberModeNodePort && selected.NodePort == 0 {
		return newRouteBuildError(routeErrorUnsupported, "backend Service port %d has no allocated NodePort", port)
	}
	return nil
}
