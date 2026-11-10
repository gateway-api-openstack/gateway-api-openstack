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

package model

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ResolveBackends resolves each declared backend without redistributing its weight.
// Input objects are immutable observations. This function performs no I/O and
// neither selects physical members nor authorizes a new forwarding mode.
func ResolveBackends(route *gatewayv1.HTTPRoute, rule gatewayv1.HTTPRouteRule, input Input) []BackendGroup {
	groups := make([]BackendGroup, 0, len(rule.BackendRefs))
	for _, reference := range rule.BackendRefs {
		groups = append(groups, resolveBackend(route, reference, input))
	}
	return groups
}

func resolveBackend(route *gatewayv1.HTTPRoute, reference gatewayv1.HTTPBackendRef, input Input) BackendGroup {
	group := BackendGroup{
		Reference: *reference.DeepCopy(), Weight: ptr.Deref(reference.Weight, 1),
		Authorization: BackendAuthorizationNotChecked, State: BackendInvalid,
		Endpoints: []Endpoint{},
	}
	serviceKey, supported := BackendServiceReference(route.Namespace, reference.BackendObjectReference)
	if !supported {
		group.Diagnostic = backendDiagnostic(gatewayv1.RouteReasonInvalidKind,
			fmt.Sprintf("backendRef group %q and kind %q are unsupported", ptr.Deref(reference.Group, ""), ptr.Deref(reference.Kind, "Service")))
		return group
	}
	group.Service = serviceKey
	if !BackendReferenceAllowed(route.Namespace, group.Service, input.ReferenceGrants) {
		group.Authorization = BackendAuthorizationDenied
		group.Diagnostic = backendDiagnostic(gatewayv1.RouteReasonRefNotPermitted,
			fmt.Sprintf("backend Service %s is not permitted by a ReferenceGrant", group.Service))
		return group
	}
	group.Authorization = BackendAuthorizationAllowed
	service := findBackendService(group.Service, input.Services)
	if service == nil {
		group.Diagnostic = backendDiagnostic(gatewayv1.RouteReasonBackendNotFound, fmt.Sprintf("backend Service %s was not found", group.Service))
		return group
	}
	group.ServiceUID = service.UID
	if service.Spec.Type == corev1.ServiceTypeLoadBalancer || service.Spec.Type == corev1.ServiceTypeExternalName {
		group.Diagnostic = backendDiagnostic(gatewayv1.RouteReasonUnsupportedValue, "backend Service must use ClusterIP or NodePort type")
		return group
	}
	port, diagnostic := ResolveServicePort(service, reference.Port)
	if diagnostic != nil {
		group.Diagnostic = diagnostic
		return group
	}
	group.Port = port.DeepCopy()
	group.Port.Protocol = corev1.ProtocolTCP
	group.State = BackendResolved
	group.Endpoints = resolveServiceEndpoints(service, port, input.EndpointSlices)
	return group
}

// BackendServiceReference normalizes a core Service reference using its source namespace.
// Other groups and kinds remain unsupported rather than being interpreted as Services.
func BackendServiceReference(namespace string, reference gatewayv1.BackendObjectReference) (types.NamespacedName, bool) {
	if ptr.Deref(reference.Group, "") != "" || ptr.Deref(reference.Kind, "Service") != "Service" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{
		Namespace: string(ptr.Deref(reference.Namespace, gatewayv1.Namespace(namespace))),
		Name:      string(reference.Name),
	}, true
}

// BackendReferenceAllowed checks Service reference authorization against observed grants.
// It performs no reads and does not confer ownership of the referenced Service.
func BackendReferenceAllowed(namespace string, target types.NamespacedName, grants []gatewayv1.ReferenceGrant) bool {
	if namespace == target.Namespace {
		return true
	}
	for _, grant := range grants {
		if grant.Namespace != target.Namespace || !grant.DeletionTimestamp.IsZero() {
			continue
		}
		fromAllowed := slices.ContainsFunc(grant.Spec.From, func(from gatewayv1.ReferenceGrantFrom) bool {
			return from.Group == gatewayv1.GroupName && from.Kind == "HTTPRoute" && string(from.Namespace) == namespace
		})
		if !fromAllowed {
			continue
		}
		if slices.ContainsFunc(grant.Spec.To, func(to gatewayv1.ReferenceGrantTo) bool {
			return to.Group == "" && to.Kind == "Service" && (to.Name == nil || string(*to.Name) == target.Name)
		}) {
			return true
		}
	}
	return false
}

func findBackendService(key types.NamespacedName, services []corev1.Service) *corev1.Service {
	for index := range services {
		service := &services[index]
		if service.Namespace == key.Namespace && service.Name == key.Name && service.DeletionTimestamp.IsZero() {
			return service
		}
	}
	return nil
}

// ResolveServicePort resolves the referenced TCP port and HTTP appProtocol.
func ResolveServicePort(service *corev1.Service, selected *gatewayv1.PortNumber) (*corev1.ServicePort, *Diagnostic) {
	var unsupportedProtocol bool
	for index := range service.Spec.Ports {
		port := &service.Spec.Ports[index]
		if selected == nil || port.Port != int32(*selected) {
			continue
		}
		if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
			unsupportedProtocol = true
			continue
		}
		if port.AppProtocol != nil && *port.AppProtocol != "http" && *port.AppProtocol != "kubernetes.io/http" {
			return nil, backendDiagnostic(gatewayv1.RouteReasonUnsupportedProtocol, "backend Service appProtocol is not supported for HTTP forwarding")
		}
		return port, nil
	}
	if unsupportedProtocol {
		return nil, backendDiagnostic(gatewayv1.RouteReasonUnsupportedProtocol, "backend Service port must use TCP")
	}
	return nil, backendDiagnostic(gatewayv1.RouteReasonBackendNotFound,
		fmt.Sprintf("backend Service %s/%s does not expose the referenced port", service.Namespace, service.Name))
}

func resolveServiceEndpoints(service *corev1.Service, servicePort *corev1.ServicePort, endpointSlices []discoveryv1.EndpointSlice) []Endpoint {
	observations := map[Endpoint]struct{}{}
	for _, endpointSlice := range endpointSlices {
		if !endpointSlice.DeletionTimestamp.IsZero() || endpointSlice.Namespace != service.Namespace || endpointSlice.Labels[discoveryv1.LabelServiceName] != service.Name {
			continue
		}
		if endpointSlice.AddressType != discoveryv1.AddressTypeIPv4 && endpointSlice.AddressType != discoveryv1.AddressTypeIPv6 {
			continue
		}
		if len(service.Spec.IPFamilies) != 0 && !slices.Contains(service.Spec.IPFamilies, corev1.IPFamily(endpointSlice.AddressType)) {
			continue
		}
		port := endpointPort(endpointSlice.Ports, servicePort.Name)
		if port != 0 {
			collectEndpointObservations(endpointSlice, port, observations)
		}
	}
	endpoints := make([]Endpoint, 0, len(observations))
	for endpoint := range observations {
		endpoints = append(endpoints, endpoint)
	}
	slices.SortFunc(endpoints, compareEndpoints)
	return endpoints
}

func endpointPort(ports []discoveryv1.EndpointPort, name string) int32 {
	for _, port := range ports {
		if ptr.Deref(port.Name, "") == name && ptr.Deref(port.Protocol, corev1.ProtocolTCP) == corev1.ProtocolTCP && ptr.Deref(port.Port, 0) > 0 {
			return *port.Port
		}
	}
	return 0
}

func collectEndpointObservations(endpointSlice discoveryv1.EndpointSlice, port int32, observations map[Endpoint]struct{}) {
	for _, endpoint := range endpointSlice.Endpoints {
		for _, address := range endpoint.Addresses {
			ip, err := netip.ParseAddr(address)
			if err != nil {
				continue
			}
			ip = ip.Unmap()
			if ip.Is4() != (endpointSlice.AddressType == discoveryv1.AddressTypeIPv4) {
				continue
			}
			observations[Endpoint{
				Address: ip.String(), Port: port, AddressType: endpointSlice.AddressType,
				Ready: ptr.Deref(endpoint.Conditions.Ready, true), Serving: ptr.Deref(endpoint.Conditions.Serving, true),
				Terminating: ptr.Deref(endpoint.Conditions.Terminating, false),
			}] = struct{}{}
		}
	}
}

func compareEndpoints(left, right Endpoint) int {
	return cmp.Or(
		cmp.Compare(left.Address, right.Address), cmp.Compare(left.Port, right.Port), cmp.Compare(left.AddressType, right.AddressType),
		compareEndpointCondition(left.Ready, right.Ready), compareEndpointCondition(left.Serving, right.Serving),
		compareEndpointCondition(left.Terminating, right.Terminating),
	)
}

func compareEndpointCondition(left, right bool) int {
	if left == right {
		return 0
	}
	if left {
		return 1
	}
	return -1
}

func backendDiagnostic(reason gatewayv1.RouteConditionReason, message string) *Diagnostic {
	return &Diagnostic{Condition: string(gatewayv1.RouteConditionResolvedRefs), Reason: string(reason), Message: message}
}
