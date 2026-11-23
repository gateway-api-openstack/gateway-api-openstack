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
