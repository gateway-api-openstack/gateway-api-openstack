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
	"fmt"
	"slices"
	"sort"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/octavia"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
)

// buildGatewayGraphPlan is free of I/O. No plan is executable unless every
// component passed validation, even when an earlier prerequisite needs repair.
func (p *Provider) buildGatewayGraphPlan(desired desiredGatewayGraph, actual observedGatewayGraph) ([]graphMutation, cloud.GatewayGraphResult, error) {
	result := cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome(), Gateway: cloud.GatewayReadyResult(actual.state)}
	if desired.spec.Gateway.Disposition == cloud.GraphDelete {
		plan := gatewayGraphDeletionPlan(actual)
		if len(plan) != 0 {
			result.Outcome = p.progressingOutcome(plan[0].message)
			result.Gateway.Outcome = result.Outcome
		}
		for _, node := range desired.spec.Routes {
			result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: node.Spec.Identity, Result: cloud.RouteResult{Outcome: result.Outcome}})
		}
		return plan, result, nil
	}
	plan, err := buildGatewayPrerequisitePlan(desired, actual)
	if err != nil {
		return nil, result, err
	}
	externalAddressReady := actual.floatingIP == nil && desired.spec.Gateway.Spec.ExternalNetworkID == "" ||
		actual.floatingIP != nil && actual.floatingIP.PortID != ""
	gatewayReady := actual.loadBalancer != nil && actual.loadBalancer.AdminStateUp && gatewayListenersReady(desired.spec.Gateway.Spec, actual) && externalAddressReady && len(plan) == 0
	if !gatewayReady {
		result.Gateway.Outcome = p.progressingOutcome("Waiting for the Gateway infrastructure")
	}
	usage := gatewayListenerUsage(desired.spec.Routes, actual.routes)
	context := routePlanContext{actual: actual, gatewayReady: gatewayReady, preservedListeners: usage.preserved}
	var deletions, ensures []graphMutation
	for index, node := range desired.spec.Routes {
		routePlan, err := p.buildGatewayRoutePlan(index, node, desired.routes[index], context)
		if routePlan.removesForwarding {
			result.Gateway.Outcome = p.progressingOutcome("Removing obsolete forwarding resources")
		}
		if err != nil {
			return nil, result, err
		}
		deletions = append(deletions, routePlan.deletions...)
		ensures = append(ensures, routePlan.ensures...)
		result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: node.Spec.Identity, Result: routePlan.result})
		if routePlan.result.Outcome.State == cloud.OutcomeProgressing {
			result.Outcome = routePlan.result.Outcome
		}
	}
	// Each rule contributes its first dependency-safe mutation. Matches from
	// different rules can interleave, and Octavia clamps out-of-range positions.
	sort.SliceStable(ensures, func(i, j int) bool { return ensures[i].priority < ensures[j].priority })
	plan = append(plan, deletions...)
	plan = append(plan, ensures...)
	if desired.spec.Gateway.Disposition == cloud.GraphEnsure {
		for _, listener := range obsoleteGatewayListeners(desired.spec.Gateway.Spec, actual.state) {
			result.Gateway.Outcome = p.progressingOutcome("Removing obsolete listeners")
			if !usage.occupied[listener.Key] {
				plan = append(plan, graphMutation{kind: graphDeleteChild, deletion: gatewayDeletionStep{resource: gatewayDeletionListener, resourceID: listener.ID}, message: "Deleted obsolete Octavia listener"})
			}
		}
	}
	if !usage.unresolved {
		restore, err := buildGatewayTrafficRestorationPlan(desired, actual)
		if err != nil {
			return nil, result, err
		}
		plan = append(plan, restore...)
	}
	if len(plan) != 0 {
		result.Outcome = p.progressingOutcome(plan[0].message)
	} else if result.Gateway.Outcome.State == cloud.OutcomeProgressing && desired.spec.Gateway.Disposition == cloud.GraphEnsure {
		result.Outcome = result.Gateway.Outcome
	}
	return plan, result, nil
}

type listenerUsage struct {
	occupied   map[cloud.ListenerKey]bool
	preserved  map[cloud.ListenerKey]bool
	unresolved bool
}

// Observe listener references once. Preserved fragments block traffic repair,
// while policies from every fragment keep their parent listener from deletion.
func gatewayListenerUsage(nodes []cloud.RouteGraphNode, actual observedRoutes) listenerUsage {
	usage := listenerUsage{occupied: make(map[cloud.ListenerKey]bool), preserved: make(map[cloud.ListenerKey]bool)}
	preservedRoutes := make(map[string]bool)
	for _, node := range nodes {
		preservedRoutes[node.Spec.Identity.RouteUID] = node.Disposition == cloud.GraphPreserve
	}
	for uid, rules := range actual {
		for _, rule := range rules {
			preserved := preservedRoutes[uid]
			if preserved && (rule.pool != nil || len(rule.policies) != 0) {
				usage.unresolved = true
			}
			for _, policy := range rule.policies {
				usage.occupied[policy.key.Listener] = true
				if preserved {
					usage.preserved[policy.key.Listener] = true
				}
			}
		}
	}
	return usage
}

type routePlanContext struct {
	actual             observedGatewayGraph
	gatewayReady       bool
	preservedListeners map[cloud.ListenerKey]bool
}

type gatewayRoutePlan struct {
	result            cloud.RouteResult
	deletions         []graphMutation
	ensures           []graphMutation
	removesForwarding bool
}

func (p *Provider) buildGatewayRoutePlan(routeIndex int, node cloud.RouteGraphNode, desired []desiredRoute, context routePlanContext) (gatewayRoutePlan, error) {
	plan := gatewayRoutePlan{result: cloud.RouteReadyResult(cloud.RouteState{})}
	observations := context.actual.routes[node.Spec.Identity.RouteUID]
	wanted := make(map[int]desiredRoute, len(desired))
	indices := make([]int, 0, len(desired)+len(observations))
	for _, route := range desired {
		wanted[route.rule.Index] = route
		indices = append(indices, route.rule.Index)
	}
	for index := range observations {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	for _, ruleIndex := range slices.Compact(indices) {
		observed := observedRouteGraph{index: ruleIndex}
		if observations[ruleIndex] != nil {
			observed = *observations[ruleIndex]
		}
		state := ruleStateFromObservation(observed)
		route, exists := wanted[ruleIndex]
		var steps []routeMutation
		deleting := node.Disposition == cloud.GraphDelete || node.Disposition == cloud.GraphEnsure && !exists
		switch {
		case deleting:
			steps = buildRouteDeletionPlan(observed)
		case node.Disposition == cloud.GraphEnsure:
			if err := validateRecordedGateway(node.Spec.Gateway, context.actual.state); err != nil {
				return plan, &cloud.GraphError{Identity: node.Spec.Identity, Err: err}
			}
			route.spec.Gateway = context.actual.state
			var err error
			steps, state, err = buildRouteEnsurePlan(route, observed)
			if err != nil {
				return plan, &cloud.GraphError{Identity: node.Spec.Identity, Err: err}
			}
			if !routeListenersAvailable(route, context) {
				steps = nil
				plan.result.Outcome = p.progressingOutcome("Waiting for the complete listener graph")
			}
		}
		if len(steps) != 0 {
			plan.result.Outcome = p.progressingOutcome(steps[0].message())
			if !deleting {
				// Later operations depend on observing this mutation's result.
				steps = steps[:1]
			} else if node.Disposition == cloud.GraphEnsure {
				plan.removesForwarding = true
			}
		}
		for _, step := range steps {
			mutation := graphMutation{kind: graphRouteMutation, routeIndex: routeIndex, ruleIndex: ruleIndex, route: step, message: step.message(), priority: routeStepPriority(route, observed, step)}
			if deleting {
				plan.deletions = append(plan.deletions, mutation)
			} else {
				plan.ensures = append(plan.ensures, mutation)
			}
		}
		plan.result.State.Rules = append(plan.result.State.Rules, state)
	}
	if node.Disposition == cloud.GraphEnsure && !context.gatewayReady {
		plan.result.Outcome = p.progressingOutcome("Waiting for the Gateway infrastructure")
	}
	return plan, nil
}

func routeListenersAvailable(route desiredRoute, context routePlanContext) bool {
	if context.actual.loadBalancer == nil {
		return false
	}
	for _, policy := range route.policies {
		if context.preservedListeners[policy.key.Listener] || context.actual.listeners[policy.key.Listener] == nil {
			return false
		}
	}
	return true
}

func obsoleteGatewayListeners(desired cloud.GatewaySpec, actual cloud.GatewayState) []cloud.ListenerState {
	wanted := make(map[cloud.ListenerKey]bool, len(desired.Listeners))
	for _, listener := range desired.Listeners {
		wanted[listener.Key] = true
	}
	var obsolete []cloud.ListenerState
	for _, listener := range actual.Listeners {
		if !wanted[listener.Key] {
			obsolete = append(obsolete, listener)
		}
	}
	return obsolete
}

func routeStepPriority(route desiredRoute, observed observedRouteGraph, step routeMutation) int32 {
	if step.policy.position != 0 {
		return step.policy.position
	}
	key := step.rule.key
	if key.Kind == cloud.ResourcePredicate {
		key.Kind, key.Predicate = cloud.ResourcePolicy, ""
	}
	for _, policy := range observed.policies {
		if policy.policy.ID == step.id || policy.policy.ID == step.parentID {
			key = policy.key
			break
		}
	}
	priority := int32(0)
	for _, policy := range route.policies {
		if policy.key == key {
			return policy.position
		}
		if priority == 0 || policy.position < priority {
			priority = policy.position
		}
	}
	return priority
}

func validateRecordedGateway(recorded, actual cloud.GatewayState) error {
	if recorded.LoadBalancerID != "" && actual.LoadBalancerID != "" && recorded.LoadBalancerID != actual.LoadBalancerID {
		return fmt.Errorf("%w: route Gateway ID differs from the observed graph", cloud.ErrOwnershipConflict)
	}
	for _, listener := range recorded.Listeners {
		if id := actual.ListenerID(listener.Key); id != "" && listener.ID != "" && id != listener.ID {
			return fmt.Errorf("%w: route listener ID differs from the observed graph", cloud.ErrOwnershipConflict)
		}
	}
	return nil
}

func gatewayListenersReady(spec cloud.GatewaySpec, actual observedGatewayGraph) bool {
	for _, listener := range spec.Listeners {
		observed := actual.listeners[listener.Key]
		if observed == nil || !observed.AdminStateUp {
			return false
		}
	}
	return len(spec.Listeners) > 0
}

func buildGatewayPrerequisitePlan(desired desiredGatewayGraph, actual observedGatewayGraph) ([]graphMutation, error) {
	if desired.spec.Gateway.Disposition != cloud.GraphEnsure {
		return nil, nil
	}
	if actual.loadBalancer == nil {
		return []graphMutation{{kind: graphCreateLoadBalancer, message: "Created Octavia load balancer"}}, nil
	}
	plan := make([]graphMutation, 0)
	networkID := desired.spec.Gateway.Spec.ExternalNetworkID
	if actual.floatingIP != nil && networkID != "" && actual.floatingIP.FloatingNetworkID != networkID {
		return nil, fmt.Errorf("%w: Gateway Floating IP network differs from its binding", cloud.ErrOwnershipConflict)
	}
	if networkID == "" && actual.floatingIP != nil {
		plan = append(plan, graphMutation{kind: graphDeleteFloatingIP, message: "Removed a no-longer-configured Floating IP"})
	}
	for _, listener := range desired.spec.Gateway.Spec.Listeners {
		if actual.listeners[listener.Key] == nil {
			plan = append(plan, graphMutation{kind: graphCreateListener, listener: listener.Key, message: "Created Octavia listener"})
		}
	}
	return plan, nil
}

func buildGatewayTrafficRestorationPlan(desired desiredGatewayGraph, actual observedGatewayGraph) ([]graphMutation, error) {
	if desired.spec.Gateway.Disposition != cloud.GraphEnsure || actual.loadBalancer == nil || len(actual.listeners) == 0 {
		return nil, nil
	}
	var plan []graphMutation
	// Restoring an external address also restores traffic. Keep the same
	// complete-fragment validation boundary as administrative repair.
	if desired.spec.Gateway.Spec.ExternalNetworkID != "" && (actual.floatingIP == nil || actual.floatingIP.PortID == "") {
		if actual.state.VIPPortID == "" || actual.state.VIPAddress == "" {
			return nil, fmt.Errorf("%w: Gateway load balancer has no complete VIP association", cloud.ErrOwnershipConflict)
		}
		mutation := graphMutation{kind: graphCreateFloatingIP, message: "Allocated Floating IP"}
		if actual.floatingIP != nil {
			mutation = graphMutation{kind: graphAssociateFloatingIP, message: "Restored Floating IP association"}
		}
		plan = append(plan, mutation)
	}
	for _, listener := range desired.spec.Gateway.Spec.Listeners {
		if observed := actual.listeners[listener.Key]; observed != nil && !observed.AdminStateUp {
			plan = append(plan, graphMutation{kind: graphEnableListener, listener: listener.Key, resourceID: observed.ID, message: "Enabled Octavia listener after validating the Gateway graph"})
		}
	}
	if !actual.loadBalancer.AdminStateUp {
		plan = append(plan, graphMutation{kind: graphEnableLoadBalancer, message: "Enabled Octavia load balancer after validating the Gateway graph"})
	}
	return plan, nil
}

func gatewayGraphDeletionPlan(actual observedGatewayGraph) []graphMutation {
	plan := make([]graphMutation, 0)
	for _, step := range actual.deletion.orderedSteps() {
		message := fmt.Sprintf("Deleting controller-owned %s", step.resource)
		if step.resource == gatewayDeletionDisablePolicy {
			message = "Disabled Octavia L7 policy before deletion"
		}
		plan = append(plan, graphMutation{kind: graphDeleteChild, deletion: step, message: message})
	}
	if actual.floatingIP != nil {
		plan = append(plan, graphMutation{kind: graphDeleteFloatingIP, message: "Deleting controller-owned Floating IP"})
	}
	if actual.loadBalancer != nil {
		plan = append(plan, graphMutation{kind: graphDeleteLoadBalancer, message: "Deleting Octavia load balancer"})
	}
	return plan
}

func ruleStateFromObservation(actual observedRouteGraph) cloud.RouteRuleState {
	state := cloud.RouteRuleState{Index: actual.index}
	if actual.pool != nil {
		state.PoolID = actual.pool.ID
	}
	if actual.monitor != nil {
		state.MonitorID = actual.monitor.ID
	}
	for _, member := range actual.members {
		state.MemberIDs = append(state.MemberIDs, member.ID)
	}
	for _, policy := range actual.policies {
		state.L7PolicyIDs = append(state.L7PolicyIDs, policy.policy.ID)
		for _, rule := range policy.rules {
			state.L7RuleIDs = append(state.L7RuleIDs, rule.rule.ID)
		}
	}
	return state
}

func (p *Provider) executeGraphMutation(ctx context.Context, desired desiredGatewayGraph, actual observedGatewayGraph, step graphMutation) error {
	var err error
	spec := desired.spec.Gateway.Spec
	switch step.kind {
	case graphCreateLoadBalancer:
		tags, tagErr := desired.identity.GatewayTags(roleLoadBalancer)
		if tagErr != nil {
			return tagErr
		}
		created, createErr := p.octavia.CreateLoadBalancer(ctx, octavia.CreateLoadBalancerOpts{CreateOpts: loadbalancers.CreateOpts{
			Name: resourceName(desired.spec.Identity, roleLoadBalancer), Description: desired.identity.Description(roleLoadBalancer),
			VipSubnetID: spec.VIPSubnetID, Provider: spec.Provider, FlavorID: spec.FlavorID, AvailabilityZone: spec.AvailabilityZone,
			AdminStateUp: boolPointer(true), Tags: tags,
		}, VIPSecurityGroupIDs: spec.VIPSecurityGroupIDs})
		err = createErr
		if err == nil {
			err = p.validateLoadBalancerProject(created.ID, created.ProjectID)
		}
	case graphCreateFloatingIP:
		created, createErr := p.neutron.CreateFloatingIP(ctx, floatingips.CreateOpts{
			Description: desired.identity.GatewayDescription(roleFloatingIP), FloatingNetworkID: spec.ExternalNetworkID, PortID: actual.state.VIPPortID,
			FixedIP: actual.state.VIPAddress,
		})
		if createErr != nil {
			return fmt.Errorf("create Floating IP: %w", createErr)
		}
		return p.validateFloatingIPProject(created.ID, created.ProjectID, created.TenantID)
	case graphAssociateFloatingIP:
		return p.associateGatewayFloatingIP(ctx, actual)
	case graphDeleteFloatingIP:
		err = p.neutron.DeleteFloatingIP(ctx, actual.floatingIP.ID)
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("delete Floating IP: %w", err)
		}
		return nil
	case graphCreateListener:
		tags, tagErr := desired.identity.ChildTags(roleListener, cloud.ResourceKey{Kind: cloud.ResourceListener, Listener: step.listener})
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreateListener(ctx, listeners.CreateOpts{
			LoadbalancerID: actual.state.LoadBalancerID, Protocol: listeners.ProtocolHTTP, ProtocolPort: step.listener.Port,
			Name: resourceName(spec.Identity, step.listener.String()), Description: desired.identity.Description(roleListener), AdminStateUp: boolPointer(true), Tags: tags,
		})
	case graphRouteMutation:
		node := desired.spec.Routes[step.routeIndex]
		route := desiredRoute{identity: desired.identities[step.routeIndex], spec: node.Spec, rule: cloud.RouteRuleSpec{Index: step.ruleIndex}}
		for _, wanted := range desired.routes[step.routeIndex] {
			if wanted.rule.Index == step.ruleIndex {
				route = wanted
				break
			}
		}
		route.spec.Gateway = actual.state
		return p.executeRouteMutation(ctx, route, step.route)
	case graphEnableListener:
		return p.enableListener(ctx, step.resourceID)
	case graphEnableLoadBalancer:
		return p.enableLoadBalancer(ctx, actual.state.LoadBalancerID)
	case graphDeleteChild:
		return p.executeGatewayDeletionStep(ctx, step.deletion)
	case graphDeleteLoadBalancer:
		err = p.octavia.DeleteLoadBalancer(ctx, actual.state.LoadBalancerID)
		if isNotFound(err) {
			err = nil
		}
	default:
		return fmt.Errorf("unsupported graph mutation %d", step.kind)
	}
	if err != nil {
		return fmt.Errorf("reconcile Gateway graph: %w", classifyOctaviaMutationError(err))
	}
	return nil
}
