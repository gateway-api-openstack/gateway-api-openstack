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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	routeErrorNoMatchingHostname  routeErrorKind = "NoMatchingListenerHostname"
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

// structurallySupportedRoute is the current runtime admission boundary.
// Multiple rules remain gated until invalid-backend responses cannot fall
// through to another rule. The pure forwarding mapper supports their layout.
func structurallySupportedRoute(route *gatewayv1.HTTPRoute) bool {
	return len(route.Spec.Rules) == 1 && validateRouteStructure(route) == nil
}

func validateRouteStructure(route *gatewayv1.HTTPRoute) error {
	if len(route.Spec.Rules) == 0 {
		return newRouteBuildError(routeErrorUnsupported, "at least one HTTPRoute rule is required")
	}
	for _, rule := range route.Spec.Rules {
		if !model.RuleOptionsSupported(rule) {
			return newRouteBuildError(routeErrorUnsupported, "named rules, timeouts, retries, and session persistence are not supported")
		}
		if err := compileRouteAction(rule); err != nil {
			return err
		}
		for _, match := range rule.Matches {
			if _, err := model.NormalizeMatch(match); err != nil {
				return newRouteBuildError(routeErrorUnsupported, "%s", err)
			}
		}
		if len(rule.BackendRefs) != 1 {
			return newRouteBuildError(routeErrorUnsupported, "exactly one backendRef per rule is supported")
		}
		backend := rule.BackendRefs[0]
		if len(backend.Filters) != 0 {
			return newRouteBuildError(routeErrorUnsupported, "backendRef filters are not supported")
		}
		if backend.Weight != nil && *backend.Weight <= 0 {
			return newRouteBuildError(routeErrorUnsupported, "a single backendRef must have positive weight")
		}
		if _, supported := model.BackendServiceReference(route.Namespace, backend.BackendObjectReference); !supported {
			return newRouteBuildError(routeErrorInvalidKind, "backendRef must refer to a core Service")
		}
		if backend.Port == nil {
			return newRouteBuildError(routeErrorUnsupported, "backendRef port is required")
		}
	}
	return nil
}

func (r *Reconciler) buildRouteModelWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
) (cloud.RouteSpec, error) {
	if err := validateRouteStructure(route); err != nil {
		return cloud.RouteSpec{}, err
	}
	inputs, err := collectRouteModelInputs(ctx, reader, gateway, []gatewayv1.HTTPRoute{*route})
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	return r.buildValidatedRouteModel(ctx, reader, route, gateway, inputs)
}

func (r *Reconciler) buildRouteModelFromInputs(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
	inputs *routeModelInputs,
) (cloud.RouteSpec, error) {
	if err := validateRouteStructure(route); err != nil {
		return cloud.RouteSpec{}, err
	}
	return r.buildValidatedRouteModel(ctx, reader, route, gateway, inputs)
}

// buildValidatedRouteModel lowers an admitted route using one observed input set.
// Reader-based callers validate before collecting backend objects; callers with
// existing inputs validate at their entry point without repeating that work here.
func (r *Reconciler) buildValidatedRouteModel(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
	gateway *gatewayv1.Gateway,
	inputs *routeModelInputs,
) (cloud.RouteSpec, error) {
	topology, err := controller.StoredGatewayTopology(r.Config, gateway)
	if err != nil {
		return cloud.RouteSpec{}, err
	}
	mode := topology.Topology.MemberMode
	for _, rule := range route.Spec.Rules {
		reference := rule.BackendRefs[0].BackendObjectReference
		key, _ := model.BackendServiceReference(route.Namespace, reference)
		if !model.BackendReferenceAllowed(route.Namespace, key, inputs.input.ReferenceGrants) {
			return cloud.RouteSpec{}, newRouteBuildError(routeErrorRefNotPermitted, "backend Service %s is not permitted by a ReferenceGrant", key)
		}
		service := inputs.service(key)
		if service == nil {
			return cloud.RouteSpec{}, fmt.Errorf("get backend Service: %w", apierrors.NewNotFound(corev1.Resource("services"), key.Name))
		}
		if err := validateBackendServicePort(service, *reference.Port, mode); err != nil {
			return cloud.RouteSpec{}, err
		}
	}

	// The runtime caller selects at most one Route per Gateway. Keep this
	// admission boundary separate from the complete physical mapping, which
	// also supports several Routes and interleaved rule priorities.
	logical := inputs.selectedRoute(route)
	forwarding, err := model.LowerHTTPForwarding(logical)
	if err != nil {
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorUnsupported, "%s", err)
	}
	if len(forwarding.Routes) != 1 {
		for _, candidate := range logical.Routes {
			for _, parent := range candidate.Parents {
				if len(parent.Diagnostics) != 0 {
					return cloud.RouteSpec{}, routeAttachmentError(parent.Diagnostics[0])
				}
			}
		}
		return cloud.RouteSpec{}, newRouteBuildError(routeErrorNotAllowed, "no listener accepts this HTTPRoute")
	}
	desired := cloud.RouteSpec{
		Identity: controller.RouteIdentity(r.Config, gateway, route),
		GatewayRequirements: cloud.GatewayRequirements{
			Provider:          "amphora",
			VIPSubnetID:       topology.Topology.VIPSubnetID,
			ExternalNetworkID: topology.Topology.ExternalNetworkID,
			FlavorID:          topology.Topology.FlavorID, AvailabilityZone: topology.Topology.AvailabilityZone,
			VIPSecurityGroupIDs: slices.Clone(topology.Topology.VIPSecurityGroupIDs),
		},
		MemberMode:              mode,
		MemberSubnetID:          topology.Topology.MemberSubnetID,
		BackendSecurityGroupIDs: slices.Clone(topology.Topology.BackendSecurityGroupIDs),
		HealthMonitor:           topology.HealthMonitor,
	}
	var nodes *corev1.NodeList
	for _, rule := range forwarding.Routes[0].Rules {
		service := inputs.service(rule.Backend.Service)
		lowered := rule.Spec
		if mode == cloud.MemberModeEndpoint {
			lowered.Members, err = endpointMembers(service, rule.Backend.Endpoints)
		} else {
			localNodes, nodeErr := readyBackendNodes(service, rule.Backend.Port, inputs.endpointSlices(rule.Backend.Service))
			if nodeErr != nil {
				return cloud.RouteSpec{}, nodeErr
			}
			if nodes == nil {
				nodes = &corev1.NodeList{}
				if err := reader.List(ctx, nodes); err != nil {
					return cloud.RouteSpec{}, fmt.Errorf("list backend Nodes: %w", err)
				}
			}
			lowered.Members, err = nodePortMembersForNodes(service, rule.Backend.Port.NodePort, topology.NodeAddressType, localNodes, nodes.Items)
		}
		if err != nil {
			return cloud.RouteSpec{}, err
		}
		desired.Rules = append(desired.Rules, lowered)
	}
	return desired, nil
}

func routeAttachmentError(diagnostic model.Diagnostic) error {
	kind := routeErrorNotAllowed
	switch diagnostic.Reason {
	case string(gatewayv1.RouteReasonUnsupportedValue):
		kind = routeErrorUnsupported
	case string(gatewayv1.RouteReasonNoMatchingParent):
		kind = routeErrorNoMatchingParent
	case string(gatewayv1.RouteReasonNoMatchingListenerHostname):
		kind = routeErrorNoMatchingHostname
	}
	return newRouteBuildError(kind, "%s", diagnostic.Message)
}
