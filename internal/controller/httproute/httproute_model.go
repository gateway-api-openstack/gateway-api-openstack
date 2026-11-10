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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
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

func structurallySupportedRoute(route *gatewayv1.HTTPRoute) bool {
	if len(route.Spec.ParentRefs) != 1 {
		return false
	}
	_, err := validateRouteStructure(route)
	return err == nil
}

// validateRouteStructure limits programming to the existing Octavia mapping.
// The logical compiler can represent a wider configuration without enabling it.
func validateRouteStructure(route *gatewayv1.HTTPRoute) (gatewayv1.BackendObjectReference, error) {
	if len(route.Spec.Hostnames) > 1 {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorUnsupported, "at most one exact hostname is supported")
	}
	if len(route.Spec.Hostnames) == 1 && strings.HasPrefix(string(route.Spec.Hostnames[0]), "*.") {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorUnsupported, "wildcard hostnames are not supported")
	}
	if len(route.Spec.Rules) != 1 {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorUnsupported, "exactly one HTTPRoute rule is supported")
	}
	rule := route.Spec.Rules[0]
	if rule.Name != nil || rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorUnsupported, "named rules, timeouts, retries, and session persistence are not supported")
	}
	if err := compileRouteAction(rule); err != nil {
		return gatewayv1.BackendObjectReference{}, err
	}
	_, _, err := supportedMatch(rule.Matches)
	if err != nil {
		return gatewayv1.BackendObjectReference{}, err
	}
	if len(rule.BackendRefs) != 1 {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorUnsupported, "exactly one backendRef is supported")
	}
	backend := rule.BackendRefs[0]
	if len(backend.Filters) != 0 {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorUnsupported, "backendRef filters are not supported")
	}
	if backend.Weight != nil && *backend.Weight == 0 {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorUnsupported, "a single backendRef must have non-zero weight")
	}
	if backend.Group != nil && *backend.Group != gatewayv1.Group("") {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorInvalidKind, "backendRef group %q is unsupported; use a core Service", *backend.Group)
	}
	if backend.Kind != nil && *backend.Kind != gatewayv1.Kind("Service") {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorInvalidKind, "backendRef kind %q is unsupported; use Service", *backend.Kind)
	}
	backendNamespace := route.Namespace
	if backend.Namespace != nil {
		backendNamespace = string(*backend.Namespace)
	}
	if backendNamespace != route.Namespace {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorRefNotPermitted, "cross-namespace backendRefs require ReferenceGrant and are deferred until Phase 5")
	}
	if backend.Port == nil {
		return gatewayv1.BackendObjectReference{}, newRouteBuildError(routeErrorUnsupported, "backendRef port is required")
	}

	return backend.BackendObjectReference, nil
}

func (r *Reconciler) buildRouteModelWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
) (cloud.RouteSpec, error) {
	if _, err := validateRouteStructure(route); err != nil {
		return cloud.RouteSpec{}, err
	}
	inputs, err := collectRouteModelInputs(ctx, reader, gateway, []gatewayv1.HTTPRoute{*route})
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	return r.buildRouteModelFromInputs(ctx, reader, route, gateway, inputs)
}

func (r *Reconciler) buildRouteModelFromInputs(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
	inputs *routeModelInputs,
) (cloud.RouteSpec, error) {
	reference, err := validateRouteStructure(route)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	key := types.NamespacedName{Namespace: route.Namespace, Name: string(reference.Name)}
	service := inputs.service(key)
	if service == nil {
		return cloud.RouteSpec{}, fmt.Errorf("get backend Service: %w", apierrors.NewNotFound(corev1.Resource("services"), key.Name))
	}
	topology, err := controller.StoredGatewayTopology(r.Config, gateway)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	mode := topology.Topology.MemberMode
	if _, err := servicePortForBackend(service, *reference.Port, mode); err != nil {
		return cloud.RouteSpec{}, err
	}
	// Route selection is still the current single-route contract. Preserve every
	// rule and backend of that route so unsupported groups cannot disappear here.
	logical := inputs.selectedRoute(route)
	match, backend, err := supportedRouteModel(logical, mode)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	var members []cloud.Member
	if mode == cloud.MemberModeEndpoint {
		members, err = endpointMembers(service, backend.Endpoints)
	} else {
		members, err = nodePortMembersFromSlices(ctx, reader, service, backend.Port, topology.NodeAddressType, inputs.endpointSlices(key))
	}
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	return cloud.RouteSpec{
		Identity: controller.RouteIdentity(r.Config, gateway, route),
		GatewayRequirements: cloud.GatewayRequirements{
			Provider:          "amphora",
			VIPSubnetID:       topology.Topology.VIPSubnetID,
			ExternalNetworkID: topology.Topology.ExternalNetworkID,
			ListenerPort:      int(logical.Listeners[0].Port),
			FlavorID:          topology.Topology.FlavorID, AvailabilityZone: topology.Topology.AvailabilityZone,
			VIPSecurityGroupIDs: slices.Clone(topology.Topology.VIPSecurityGroupIDs),
		},
		MemberMode:              mode,
		MemberSubnetID:          topology.Topology.MemberSubnetID,
		BackendSecurityGroupIDs: slices.Clone(topology.Topology.BackendSecurityGroupIDs),
		HealthMonitor:           topology.HealthMonitor,
		Hostname:                match.Hostname,
		PathType:                cloud.PathMatchType(match.Match.PathType),
		PathValue:               match.Match.PathValue,
		Members:                 members,
	}, nil
}

// supportedRouteModel lowers only the currently implemented graph shape. Model
// diagnostics never grant authority to drop a fragment or choose a fallback.
func supportedRouteModel(logical model.Gateway, mode cloud.MemberMode) (model.RouteMatch, model.BackendGroup, error) {
	unsupported := func() (model.RouteMatch, model.BackendGroup, error) {
		return model.RouteMatch{}, model.BackendGroup{}, newRouteBuildError(routeErrorUnsupported, "logical Gateway configuration is outside the supported HTTP profile")
	}
	if len(logical.Diagnostics) != 0 || len(logical.Listeners) != 1 || len(logical.Routes) != 1 {
		return unsupported()
	}
	listener, route := logical.Listeners[0], logical.Routes[0]
	if len(listener.Diagnostics) != 0 || len(route.Diagnostics) != 0 || len(listener.Rules) != 1 || len(route.Rules) != 1 || len(route.Parents) != 1 {
		return unsupported()
	}
	match := listener.Rules[0]
	if listener.Hostname != "" || route.Source.Namespace != logical.Source.Namespace || len(match.Match.Headers) != 0 || strings.HasPrefix(match.Hostname, "*.") {
		return unsupported()
	}
	rule, parent := route.Rules[0], route.Parents[0]
	if len(rule.Diagnostics) != 0 || len(rule.Backends) != 1 || len(rule.Matches) != 1 || parent.State != model.AttachmentAccepted {
		return unsupported()
	}
	backend := rule.Backends[0]
	if backend.Diagnostic != nil || backend.Port == nil || backend.Weight == 0 || backend.Service.Namespace != route.Source.Namespace {
		return unsupported()
	}
	if mode == cloud.MemberModeNodePort && backend.Port.NodePort == 0 {
		return unsupported()
	}
	return match, backend, nil
}

func supportedMatch(matches []gatewayv1.HTTPRouteMatch) (cloud.PathMatchType, string, error) {
	if len(matches) > 1 {
		return "", "", newRouteBuildError(routeErrorUnsupported, "at most one HTTPRoute match is supported")
	}
	var match gatewayv1.HTTPRouteMatch
	if len(matches) == 1 {
		match = matches[0]
	}
	if len(match.Headers) != 0 || len(match.QueryParams) != 0 || match.Method != nil {
		return "", "", newRouteBuildError(routeErrorUnsupported, "header, query parameter, and method matches are not supported")
	}
	normalized, err := model.NormalizeMatch(match)
	if err != nil {
		return "", "", newRouteBuildError(routeErrorUnsupported, "%s", err)
	}
	return cloud.PathMatchType(normalized.PathType), normalized.PathValue, nil
}
