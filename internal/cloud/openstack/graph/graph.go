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

package graph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/octavia"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
)

type desiredGatewayGraph struct {
	spec     cloud.GatewayGraphSpec
	identity Identity
	routes   []desiredRoute
}

type observedGatewayGraph struct {
	loadBalancer *octavia.LoadBalancer
	listener     *listeners.Listener
	floatingIP   *floatingips.FloatingIP
	routes       []observedRouteGraph
	deletion     gatewayDeletionPlan
	state        cloud.GatewayState
	outcome      cloud.Outcome
}

type graphMutationKind uint8

const (
	graphCreateLoadBalancer graphMutationKind = iota
	graphCreateFloatingIP
	graphAssociateFloatingIP
	graphDeleteFloatingIP
	graphCreateListener
	graphRouteMutation
	graphEnableListener
	graphEnableLoadBalancer
	graphDeleteChild
	graphDeleteLoadBalancer
)

type graphMutation struct {
	kind       graphMutationKind
	routeIndex int
	route      routeMutation
	deletion   gatewayDeletionStep
	message    string
}

// ReconcileGatewayGraph validates and observes every component before building
// one ordered plan. It executes at most its first mutation and never caches the
// plan across calls, including after a timeout or a lost create response.
func (p *Provider) ReconcileGatewayGraph(ctx context.Context, spec cloud.GatewayGraphSpec) (result cloud.GatewayGraphResult, retErr error) {
	defer func() {
		if retErr == nil {
			return
		}
		var component *cloud.GraphError
		if errors.As(retErr, &component) {
			retErr = &cloud.GraphError{Identity: component.Identity, Err: classifyOpenStackError(component.Err)}
		} else {
			retErr = &cloud.GraphError{Identity: spec.Identity, Err: classifyOpenStackError(retErr)}
		}
	}()
	ctx, cancel := p.operationContext(ctx)
	defer cancel()

	desired, err := p.buildDesiredGatewayGraph(spec)
	if err != nil {
		return result, err
	}
	actual, err := p.observeGatewayGraph(ctx, desired)
	if err != nil {
		result.Gateway = observedGatewayResult(desired, actual, p.progressingOutcome("Waiting for the Gateway infrastructure"))
		return result, err
	}
	if actual.outcome.State == cloud.OutcomeProgressing {
		result = pendingGraphResult(desired, actual.outcome)
		result.Gateway = observedGatewayResult(desired, actual, actual.outcome)
		return result, nil
	}
	if spec.Gateway.Disposition == cloud.GraphEnsure {
		routes := make([]cloud.RouteSpec, 0, len(desired.routes))
		for index, route := range desired.routes {
			if desired.spec.Routes[index].Disposition == cloud.GraphEnsure {
				routes = append(routes, route.spec)
			}
		}
		if err := p.topology.ValidateGatewaySecurity(ctx, desired.spec.Gateway.Spec, routes); err != nil {
			return result, err
		}
	}
	plan, result, err := p.buildGatewayGraphPlan(desired, actual)
	if err != nil {
		return result, err
	}
	if len(plan) == 0 {
		return result, nil
	}
	step := plan[0]
	if err := p.executeGraphMutation(ctx, desired, actual, step); err != nil {
		if step.kind == graphRouteMutation {
			return result, &cloud.GraphError{Identity: desired.routes[step.routeIndex].spec.Identity, Err: err}
		}
		return result, err
	}
	result.Outcome = p.progressingOutcome(step.message)
	return result, nil
}

func (p *Provider) buildDesiredGatewayGraph(spec cloud.GatewayGraphSpec) (desiredGatewayGraph, error) {
	identity, err := p.identity(spec.Identity)
	if err != nil {
		return desiredGatewayGraph{}, err
	}
	invalid := func(message string) (desiredGatewayGraph, error) {
		return desiredGatewayGraph{}, cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, errors.New(message))
	}
	if spec.Identity.RouteUID != "" || spec.Identity.RouteName != "" || spec.Identity.RouteNamespace != "" {
		return invalid("graph identity must not contain HTTPRoute fields")
	}
	if !validGraphDisposition(spec.Gateway.Disposition) {
		return invalid("Gateway graph disposition is invalid")
	}
	if spec.Gateway.Disposition == cloud.GraphEnsure {
		if !sameGatewayIdentity(spec.Identity, spec.Gateway.Spec.Identity) {
			return desiredGatewayGraph{}, fmt.Errorf("%w: Gateway model and graph identities differ", cloud.ErrOwnershipConflict)
		}
		if spec.Gateway.Spec.Provider != "amphora" || strings.TrimSpace(spec.Gateway.Spec.VIPSubnetID) == "" ||
			spec.Gateway.Spec.ListenerPort < 1 || spec.Gateway.Spec.ListenerPort > 65535 {
			return invalid("Gateway requires Amphora, a VIP subnet, and an HTTP listener port within 1-65535")
		}
		for _, role := range []string{roleLoadBalancer, roleListener} {
			if _, err := identity.GatewayTags(role); err != nil {
				return desiredGatewayGraph{}, err
			}
		}
	}
	// Copy before sorting: callers can reuse their immutable reconciliation input.
	spec.Routes = append([]cloud.RouteGraphNode(nil), spec.Routes...)
	sort.Slice(spec.Routes, func(i, j int) bool {
		a, b := spec.Routes[i].Spec.Identity, spec.Routes[j].Spec.Identity
		if a.RouteNamespace != b.RouteNamespace {
			return a.RouteNamespace < b.RouteNamespace
		}
		if a.RouteName != b.RouteName {
			return a.RouteName < b.RouteName
		}
		return a.RouteUID < b.RouteUID
	})
	desired := desiredGatewayGraph{spec: spec, identity: identity}
	identities := make(map[string]struct{}, len(spec.Routes))
	ensured := 0
	for _, node := range spec.Routes {
		id := node.Spec.Identity
		if !validGraphDisposition(node.Disposition) {
			return invalid("HTTPRoute graph disposition is invalid")
		}
		if err := cloud.ValidateRouteIdentity(id); err != nil {
			return desiredGatewayGraph{}, &cloud.GraphError{Identity: id, Err: err}
		}
		if !sameGatewayIdentity(spec.Identity, id) {
			return desiredGatewayGraph{}, &cloud.GraphError{Identity: id, Err: fmt.Errorf("%w: HTTPRoute belongs to a different Gateway graph", cloud.ErrOwnershipConflict)}
		}
		if _, duplicate := identities[id.RouteUID]; duplicate {
			return desiredGatewayGraph{}, &cloud.GraphError{Identity: id, Err: fmt.Errorf("%w: duplicate HTTPRoute fragment", cloud.ErrOwnershipConflict)}
		}
		identities[id.RouteUID] = struct{}{}
		routeIdentity, err := p.identity(id)
		if err != nil {
			return desiredGatewayGraph{}, err
		}
		route := desiredRoute{identity: routeIdentity, spec: node.Spec}
		if node.Disposition == cloud.GraphEnsure {
			ensured++
			if spec.Gateway.Disposition != cloud.GraphEnsure {
				return invalid("an ensured HTTPRoute requires an ensured Gateway")
			}
			if !node.Spec.GatewayRequirements.Equal(gatewayRequirements(spec.Gateway.Spec)) {
				return desiredGatewayGraph{}, &cloud.GraphError{Identity: id, Err: fmt.Errorf("%w: HTTPRoute and Gateway infrastructure requirements differ", cloud.ErrOwnershipConflict)}
			}
			route, err = p.buildDesiredRoute(node.Spec)
			if err != nil {
				return desiredGatewayGraph{}, &cloud.GraphError{Identity: id, Err: err}
			}
		}
		desired.routes = append(desired.routes, route)
	}
	if ensured > 1 {
		return invalid("the current HTTP profile supports one ensured HTTPRoute")
	}
	return desired, nil
}

func observedGatewayResult(desired desiredGatewayGraph, actual observedGatewayGraph, pending cloud.Outcome) cloud.GatewayResult {
	result := cloud.GatewayResult{State: actual.state, Outcome: pending}
	if actual.loadBalancer == nil || actual.listener == nil || !actual.loadBalancer.AdminStateUp || !actual.listener.AdminStateUp {
		return result
	}
	if desired.spec.Gateway.Disposition == cloud.GraphEnsure {
		networkID := desired.spec.Gateway.Spec.ExternalNetworkID
		if networkID == "" && actual.floatingIP != nil || networkID != "" && (actual.floatingIP == nil || actual.floatingIP.FloatingNetworkID != networkID || actual.floatingIP.PortID == "") {
			return result
		}
		result.Outcome = cloud.ReadyOutcome()
	}
	return result
}

func validGraphDisposition(disposition cloud.GraphDisposition) bool {
	return disposition == cloud.GraphEnsure || disposition == cloud.GraphPreserve || disposition == cloud.GraphDelete
}

func sameGatewayIdentity(a, b cloud.Identity) bool {
	return a.OpenStackProjectID == b.OpenStackProjectID && a.ClusterID == b.ClusterID && a.Controller == b.Controller &&
		a.GatewayNamespace == b.GatewayNamespace && a.GatewayName == b.GatewayName && a.GatewayUID == b.GatewayUID
}

func gatewayRequirements(spec cloud.GatewaySpec) cloud.GatewayRequirements {
	return cloud.GatewayRequirements{Provider: spec.Provider, VIPSubnetID: spec.VIPSubnetID, ExternalNetworkID: spec.ExternalNetworkID,
		FlavorID: spec.FlavorID, AvailabilityZone: spec.AvailabilityZone, ListenerPort: spec.ListenerPort, VIPSecurityGroupIDs: spec.VIPSecurityGroupIDs}
}

func (p *Provider) observeGatewayGraph(ctx context.Context, desired desiredGatewayGraph) (observedGatewayGraph, error) {
	actual := observedGatewayGraph{outcome: cloud.ReadyOutcome()}
	lb, err := p.findGatewayLoadBalancer(ctx, desired.identity)
	if err != nil {
		return actual, err
	}
	if lb == nil {
		return p.observeAbsentGateway(ctx, desired)
	}
	if !desired.identity.MatchesGateway(lb.Tags, roleLoadBalancer) {
		return actual, fmt.Errorf("%w: load balancer %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, lb.ID)
	}
	observation, err := p.observeLoadBalancerOnce(ctx, lb.ID)
	if err != nil {
		return actual, err
	}
	if observation.loadBalancer != nil {
		observed := observation.loadBalancer
		if observed.ID != lb.ID || !desired.identity.MatchesGateway(observed.Tags, roleLoadBalancer) {
			return actual, fmt.Errorf("%w: load balancer identity changed during observation", cloud.ErrOwnershipConflict)
		}
		if err := p.validateLoadBalancerProject(observed.ID, observed.ProjectID); err != nil {
			return actual, err
		}
	}
	if observation.phase == loadBalancerPhaseAbsent {
		return p.observeAbsentGateway(ctx, desired)
	}
	actual.outcome = observation.outcome
	actual.loadBalancer = observation.loadBalancer
	lb = actual.loadBalancer
	actual.state = cloud.GatewayState{LoadBalancerID: lb.ID, VIPPortID: lb.VipPortID, VIPAddress: lb.VipAddress}
	if observation.phase == loadBalancerPhasePending && desired.spec.Gateway.Disposition != cloud.GraphEnsure {
		return actual, nil
	}
	if desired.spec.Gateway.Disposition == cloud.GraphEnsure {
		if err := validateGraphLoadBalancer(lb, gatewayRequirements(desired.spec.Gateway.Spec)); err != nil {
			return actual, err
		}
	}
	actual.floatingIP, err = p.findGatewayFloatingIP(ctx, desired.identity, lb.VipPortID, lb.VipAddress)
	if err != nil {
		return actual, err
	}
	if actual.floatingIP != nil {
		actual.state.FloatingIPID = actual.floatingIP.ID
		if actual.floatingIP.PortID != "" {
			actual.state.FloatingIPAddress = actual.floatingIP.FloatingIP
		}
	}
	if desired.spec.Gateway.Disposition == cloud.GraphDelete {
		if observation.phase == loadBalancerPhasePending {
			return actual, nil
		}
		actual.deletion, err = p.buildGatewayDeletionPlan(ctx, desired.identity, lb.ID)
		return actual, err
	}
	actual.listener, err = p.findManagedGatewayListener(ctx, desired.identity, lb.ID)
	if err != nil {
		return actual, err
	}
	if actual.listener != nil {
		if actual.listener.Protocol != string(listeners.ProtocolHTTP) {
			return actual, fmt.Errorf("%w: Gateway listener does not use HTTP", cloud.ErrOwnershipConflict)
		}
		actual.state.ListenerID = actual.listener.ID
		if desired.spec.Gateway.Disposition == cloud.GraphEnsure && actual.listener.ProtocolPort != desired.spec.Gateway.Spec.ListenerPort {
			return actual, fmt.Errorf("%w: Gateway listener port differs from its binding", cloud.ErrOwnershipConflict)
		}
		if len(lb.VIPSecurityGroupIDs) > 0 && len(actual.listener.AllowedCIDRs) > 0 {
			return actual, fmt.Errorf("%w: custom VIP security groups cannot be combined with listener allowed CIDRs", cloud.ErrOwnershipConflict)
		}
	}
	// Octavia's parent provisioning status also covers route mutations. Observe
	// the immutable Gateway component without changing it so member progress does
	// not erase a still-programmed Gateway from component status.
	if observation.phase == loadBalancerPhasePending {
		return actual, nil
	}
	identities := make([]Identity, len(desired.routes))
	for index, route := range desired.routes {
		identities[index] = route.identity
	}
	actual.routes, err = p.observeGatewayRoutes(ctx, identities, actual.state)
	return actual, err
}

// observeAbsentGateway checks independently allocated Neutron resources before
// declaring a deleted graph absent or creating a replacement load balancer.
func (p *Provider) observeAbsentGateway(ctx context.Context, desired desiredGatewayGraph) (observedGatewayGraph, error) {
	actual := observedGatewayGraph{outcome: cloud.ReadyOutcome()}
	floatingIP, err := p.findGatewayFloatingIP(ctx, desired.identity, "", "")
	if err != nil {
		return actual, err
	}
	actual.floatingIP = floatingIP
	if floatingIP == nil {
		return actual, nil
	}
	actual.state.FloatingIPID = floatingIP.ID
	if desired.spec.Gateway.Disposition == cloud.GraphEnsure {
		return actual, fmt.Errorf("%w: Gateway Floating IP remains after its load balancer disappeared", cloud.ErrOwnershipConflict)
	}
	return actual, nil
}

func validateGraphLoadBalancer(lb *octavia.LoadBalancer, requirements cloud.GatewayRequirements) error {
	if lb.Provider != requirements.Provider || lb.VipSubnetID != requirements.VIPSubnetID ||
		requirements.FlavorID != "" && lb.FlavorID != requirements.FlavorID ||
		requirements.AvailabilityZone != "" && lb.AvailabilityZone != requirements.AvailabilityZone {
		return fmt.Errorf("%w: load balancer %s differs from the bound topology", cloud.ErrOwnershipConflict, lb.ID)
	}
	groups := slices.Clone(lb.VIPSecurityGroupIDs)
	slices.Sort(groups)
	if !slices.Equal(groups, requirements.VIPSecurityGroupIDs) {
		return fmt.Errorf("%w: load balancer VIP security groups differ from the bound topology", cloud.ErrOwnershipConflict)
	}
	return nil
}

func pendingGraphResult(desired desiredGatewayGraph, outcome cloud.Outcome) cloud.GatewayGraphResult {
	result := cloud.GatewayGraphResult{Outcome: outcome, Gateway: cloud.GatewayResult{Outcome: outcome}}
	for _, route := range desired.routes {
		result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: route.spec.Identity, Result: cloud.RouteResult{Outcome: outcome}})
	}
	return result
}
