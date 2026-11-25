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
	spec       cloud.GatewayGraphSpec
	identity   Identity
	routes     [][]desiredRoute
	identities []Identity
}

type observedGatewayGraph struct {
	loadBalancer *octavia.LoadBalancer
	listeners    map[cloud.ListenerKey]*listeners.Listener
	floatingIP   *floatingips.FloatingIP
	routes       observedRoutes
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
	ruleIndex  int
	priority   int32
	listener   cloud.ListenerKey
	resourceID string
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
		routes := make([]cloud.RouteSpec, 0, len(desired.spec.Routes))
		for _, node := range desired.spec.Routes {
			if node.Disposition == cloud.GraphEnsure {
				routes = append(routes, node.Spec)
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
			return result, &cloud.GraphError{Identity: desired.spec.Routes[step.routeIndex].Spec.Identity, Err: err}
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
	listenerKeys := make(map[cloud.ListenerKey]struct{})
	if spec.Gateway.Disposition == cloud.GraphEnsure {
		if !sameGatewayIdentity(spec.Identity, spec.Gateway.Spec.Identity) {
			return desiredGatewayGraph{}, fmt.Errorf("%w: Gateway model and graph identities differ", cloud.ErrOwnershipConflict)
		}
		if spec.Gateway.Spec.Provider != "amphora" || strings.TrimSpace(spec.Gateway.Spec.VIPSubnetID) == "" || len(spec.Gateway.Spec.Listeners) == 0 {
			return invalid("Gateway requires Amphora, a VIP subnet, and HTTP listeners")
		}
		for _, listener := range spec.Gateway.Spec.Listeners {
			if err := listener.Key.Validate(); err != nil {
				return invalid(err.Error())
			}
			if _, duplicate := listenerKeys[listener.Key]; duplicate {
				return invalid("duplicate physical listener key")
			}
			listenerKeys[listener.Key] = struct{}{}
			if _, err := identity.ChildTags(roleListener, cloud.ResourceKey{Kind: cloud.ResourceListener, Listener: listener.Key}); err != nil {
				return invalid(err.Error())
			}
		}
		if _, err := identity.GatewayTags(roleLoadBalancer); err != nil {
			return invalid(err.Error())
		}
	}
	spec.Gateway.Spec.Listeners = slices.Clone(spec.Gateway.Spec.Listeners)
	sort.Slice(spec.Gateway.Spec.Listeners, func(i, j int) bool {
		return spec.Gateway.Spec.Listeners[i].Key.String() < spec.Gateway.Spec.Listeners[j].Key.String()
	})
	spec.Routes = slices.Clone(spec.Routes)
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
	desired := desiredGatewayGraph{spec: spec, identity: identity, routes: make([][]desiredRoute, len(spec.Routes))}
	identities := make(map[string]struct{}, len(spec.Routes))
	priorities := make(map[cloud.ListenerKey]map[int]struct{})
	policies := make(map[cloud.ListenerKey][]*desiredPolicy)
	for index, node := range spec.Routes {
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
			return desiredGatewayGraph{}, fmt.Errorf("%w: duplicate HTTPRoute fragment", cloud.ErrOwnershipConflict)
		}
		identities[id.RouteUID] = struct{}{}
		routeIdentity, err := p.identity(id)
		if err != nil {
			return desiredGatewayGraph{}, err
		}
		desired.identities = append(desired.identities, routeIdentity)
		if node.Disposition != cloud.GraphEnsure {
			continue
		}
		if spec.Gateway.Disposition != cloud.GraphEnsure {
			return invalid("an ensured HTTPRoute requires an ensured Gateway")
		}
		if !node.Spec.GatewayRequirements.Equal(gatewayRequirements(spec.Gateway.Spec)) {
			return desiredGatewayGraph{}, &cloud.GraphError{Identity: id, Err: fmt.Errorf("%w: HTTPRoute and Gateway infrastructure requirements differ", cloud.ErrOwnershipConflict)}
		}
		if len(node.Spec.Rules) == 0 {
			return invalid("ensured HTTPRoute requires forwarding rules")
		}
		ruleIndexes := make(map[int]struct{})
		for _, rule := range node.Spec.Rules {
			if _, duplicate := ruleIndexes[rule.Index]; duplicate {
				return invalid("duplicate HTTPRoute rule index")
			}
			ruleIndexes[rule.Index] = struct{}{}
			for _, match := range rule.Matches {
				if _, present := listenerKeys[match.Listener]; !present {
					return invalid("route match references an unknown physical listener")
				}
				if priorities[match.Listener] == nil {
					priorities[match.Listener] = make(map[int]struct{})
				}
				if _, duplicate := priorities[match.Listener][match.Priority]; duplicate {
					return invalid("listener matches must have distinct priorities")
				}
				priorities[match.Listener][match.Priority] = struct{}{}
			}
			compiled, err := p.buildDesiredRule(node.Spec, rule)
			if err != nil {
				return desiredGatewayGraph{}, &cloud.GraphError{Identity: id, Err: err}
			}
			for policyIndex := range compiled.policies {
				policy := &compiled.policies[policyIndex]
				if _, err := routeIdentity.ChildTags(policy.role, policy.key); err != nil {
					return invalid(err.Error())
				}
				for _, predicate := range rulesFor(*policy) {
					if _, err := routeIdentity.ChildTags(predicate.role, predicate.key); err != nil {
						return invalid(err.Error())
					}
				}
				policies[policy.key.Listener] = append(policies[policy.key.Listener], policy)
			}
			desired.routes[index] = append(desired.routes[index], compiled)
		}
		sort.Slice(desired.routes[index], func(i, j int) bool { return desired.routes[index][i].rule.Index < desired.routes[index][j].rule.Index })
	}
	for _, items := range policies {
		sort.SliceStable(items, func(i, j int) bool { return items[i].match.Priority < items[j].match.Priority })
		for index, policy := range items {
			policy.position = int32(index + 1)
		}
	}
	return desired, nil
}

func observedGatewayResult(desired desiredGatewayGraph, actual observedGatewayGraph, pending cloud.Outcome) cloud.GatewayResult {
	result := cloud.GatewayResult{State: actual.state, Outcome: pending}
	if actual.loadBalancer == nil || !gatewayListenersReady(desired.spec.Gateway.Spec, actual) || !actual.loadBalancer.AdminStateUp {
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
		FlavorID: spec.FlavorID, AvailabilityZone: spec.AvailabilityZone, VIPSecurityGroupIDs: spec.VIPSecurityGroupIDs}
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
	actual.listeners, err = p.findManagedGatewayListeners(ctx, desired.identity, lb.ID)
	if err != nil {
		return actual, err
	}
	keys := make([]cloud.ListenerKey, 0, len(actual.listeners))
	for key := range actual.listeners {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, key := range keys {
		listener := actual.listeners[key]
		if len(lb.VIPSecurityGroupIDs) > 0 && len(listener.AllowedCIDRs) > 0 {
			return actual, fmt.Errorf("%w: custom VIP security groups cannot be combined with listener allowed CIDRs", cloud.ErrOwnershipConflict)
		}
		actual.state.Listeners = append(actual.state.Listeners, cloud.ListenerState{Key: key, ID: listener.ID})
	}
	// Octavia's parent provisioning status also covers route mutations. Observe
	// the immutable Gateway component without changing it so member progress does
	// not erase a still-programmed Gateway from component status.
	if observation.phase == loadBalancerPhasePending {
		return actual, nil
	}
	actual.routes, err = p.observeGatewayRoutes(ctx, desired.identities, actual.state)
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
	for _, node := range desired.spec.Routes {
		result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: node.Spec.Identity, Result: cloud.RouteResult{Outcome: outcome}})
	}
	return result
}
