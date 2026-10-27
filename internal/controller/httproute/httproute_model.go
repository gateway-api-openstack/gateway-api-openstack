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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

type routeErrorKind string

const (
	routeErrorUnsupported         routeErrorKind = "Unsupported"
	routeErrorInvalidKind         routeErrorKind = "InvalidKind"
	routeErrorRefNotPermitted     routeErrorKind = "RefNotPermitted"
	routeErrorUnsupportedProtocol routeErrorKind = "UnsupportedProtocol"
	routeErrorNoMatchingParent    routeErrorKind = "NoMatchingParent"
	routeErrorNotAllowed          routeErrorKind = "NotAllowedByListeners"
	routeErrorPending             routeErrorKind = "Pending"
)

type routeBuildError struct {
	kind    routeErrorKind
	message string
}

func (e *routeBuildError) Error() string { return e.message }

func newRouteBuildError(kind routeErrorKind, format string, args ...any) error {
	return &routeBuildError{kind: kind, message: fmt.Sprintf(format, args...)}
}

// routeStructure contains the supported HTTPRoute fields before resolving backends.
type routeStructure struct {
	hostname  string
	pathType  cloud.PathMatchType
	pathValue string
	backend   gatewayv1.BackendObjectReference
}

func structurallySupportedRoute(route *gatewayv1.HTTPRoute) bool {
	if len(route.Spec.ParentRefs) != 1 {
		return false
	}
	_, err := parseRouteStructure(route)
	return err == nil
}

func parseRouteStructure(route *gatewayv1.HTTPRoute) (routeStructure, error) {
	if len(route.Spec.Hostnames) > 1 {
		return routeStructure{}, newRouteBuildError(routeErrorUnsupported, "at most one exact hostname is supported")
	}
	hostname := ""
	if len(route.Spec.Hostnames) == 1 {
		hostname = string(route.Spec.Hostnames[0])
		if strings.HasPrefix(hostname, "*.") {
			return routeStructure{}, newRouteBuildError(routeErrorUnsupported, "wildcard hostnames are not supported")
		}
	}
	if len(route.Spec.Rules) != 1 {
		return routeStructure{}, newRouteBuildError(routeErrorUnsupported, "exactly one HTTPRoute rule is supported")
	}
	rule := route.Spec.Rules[0]
	if rule.Name != nil || rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil {
		return routeStructure{}, newRouteBuildError(routeErrorUnsupported, "named rules, timeouts, retries, and session persistence are not supported")
	}
	if err := compileRouteAction(rule); err != nil {
		return routeStructure{}, err
	}
	pathType, pathValue, err := supportedMatch(rule.Matches)
	if err != nil {
		return routeStructure{}, err
	}
	if len(rule.BackendRefs) != 1 {
		return routeStructure{}, newRouteBuildError(routeErrorUnsupported, "exactly one backendRef is supported")
	}
	backend := rule.BackendRefs[0]
	if len(backend.Filters) != 0 {
		return routeStructure{}, newRouteBuildError(routeErrorUnsupported, "backendRef filters are not supported")
	}
	if backend.Weight != nil && *backend.Weight == 0 {
		return routeStructure{}, newRouteBuildError(routeErrorUnsupported, "a single backendRef must have non-zero weight")
	}
	if backend.Group != nil && *backend.Group != gatewayv1.Group("") {
		return routeStructure{}, newRouteBuildError(routeErrorInvalidKind, "backendRef group %q is unsupported; use a core Service", *backend.Group)
	}
	if backend.Kind != nil && *backend.Kind != gatewayv1.Kind("Service") {
		return routeStructure{}, newRouteBuildError(routeErrorInvalidKind, "backendRef kind %q is unsupported; use Service", *backend.Kind)
	}
	backendNamespace := route.Namespace
	if backend.Namespace != nil {
		backendNamespace = string(*backend.Namespace)
	}
	if backendNamespace != route.Namespace {
		return routeStructure{}, newRouteBuildError(routeErrorRefNotPermitted, "cross-namespace backendRefs require ReferenceGrant and are deferred until Phase 5")
	}
	if backend.Port == nil {
		return routeStructure{}, newRouteBuildError(routeErrorUnsupported, "backendRef port is required")
	}

	return routeStructure{
		hostname: hostname, pathType: pathType, pathValue: pathValue,
		backend: backend.BackendObjectReference,
	}, nil
}

func (r *Reconciler) buildRouteSpecWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
) (cloud.RouteSpec, error) {
	structure, err := parseRouteStructure(route)
	if err != nil {
		return cloud.RouteSpec{}, err
	}

	var service corev1.Service
	if err := reader.Get(ctx, types.NamespacedName{Namespace: route.Namespace, Name: string(structure.backend.Name)}, &service); err != nil {
		return cloud.RouteSpec{}, fmt.Errorf("get backend Service: %w", err)
	}
	if service.Spec.Type != corev1.ServiceTypeNodePort {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "backend Service %s must be type NodePort", service.Name)
	}
	servicePort, err := nodePortForBackend(&service, int32(*structure.backend.Port))
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	topology, err := controller.StoredGatewayTopology(r.Config, gateway)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	members, err := nodePortMembersFromReader(ctx, reader, &service, servicePort, topology.NodeAddressType)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	return cloud.RouteSpec{
		Identity: controller.RouteIdentity(r.Config, gateway, route),
		GatewayRequirements: cloud.GatewayRequirements{
			Provider:          "amphora",
			VIPSubnetID:       topology.Topology.VIPSubnetID,
			ExternalNetworkID: topology.Topology.ExternalNetworkID,
			ListenerPort:      int(gateway.Spec.Listeners[0].Port),
			FlavorID:          topology.Topology.FlavorID, AvailabilityZone: topology.Topology.AvailabilityZone,
			VIPSecurityGroupIDs: slices.Clone(topology.Topology.VIPSecurityGroupIDs),
		},
		MemberSubnetID:          topology.Topology.MemberSubnetID,
		BackendSecurityGroupIDs: slices.Clone(topology.Topology.BackendSecurityGroupIDs),
		HealthMonitor:           topology.HealthMonitor,
		Hostname:                structure.hostname,
		PathType:                structure.pathType,
		PathValue:               structure.pathValue,
		Members:                 members,
	}, nil
}

func supportedMatch(matches []gatewayv1.HTTPRouteMatch) (cloud.PathMatchType, string, error) {
	if len(matches) == 0 {
		return cloud.PathMatchPrefix, "/", nil
	}
	if len(matches) != 1 {
		return "", "", newRouteBuildError(routeErrorUnsupported, "at most one HTTPRoute match is supported")
	}
	match := &matches[0]
	if len(match.Headers) != 0 || len(match.QueryParams) != 0 || match.Method != nil {
		return "", "", newRouteBuildError(routeErrorUnsupported, "header, query parameter, and method matches are not supported")
	}
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/"
	if match.Path != nil {
		if match.Path.Type != nil {
			pathType = *match.Path.Type
		}
		if match.Path.Value != nil {
			pathValue = *match.Path.Value
		}
	}
	if !validHTTPPath(pathValue) {
		return "", "", newRouteBuildError(routeErrorUnsupported, "path %q is not a valid absolute Gateway API path", pathValue)
	}
	switch pathType {
	case gatewayv1.PathMatchExact:
		return cloud.PathMatchExact, pathValue, nil
	case gatewayv1.PathMatchPathPrefix:
		return cloud.PathMatchPrefix, pathValue, nil
	default:
		return "", "", newRouteBuildError(routeErrorUnsupported, "path match type %q is not supported", pathType)
	}
}

func validHTTPPath(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "//") || strings.Contains(value, "/./") || strings.Contains(value, "/../") {
		return false
	}
	return value != "/." && value != "/.." && !strings.HasSuffix(value, "/.") && !strings.HasSuffix(value, "/..")
}
