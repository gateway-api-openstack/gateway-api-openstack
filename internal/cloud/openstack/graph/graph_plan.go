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

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
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
		for _, route := range desired.routes {
			result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: route.spec.Identity, Result: cloud.RouteResult{Outcome: result.Outcome}})
		}
		return plan, result, nil
	}
	plan, err := buildGatewayPrerequisitePlan(desired, actual)
	if err != nil {
		return nil, result, err
	}
	gatewayReady := actual.loadBalancer != nil && actual.listener != nil && actual.loadBalancer.AdminStateUp && actual.listener.AdminStateUp && len(plan) == 0
	if !gatewayReady {
		result.Gateway.Outcome = p.progressingOutcome("Waiting for the Gateway infrastructure")
	}
	deletions := make([]graphMutation, 0)
	ensures := make([]graphMutation, 0)
	canRestoreTraffic := true
	for index, route := range desired.routes {
		observed := observedRouteGraph{}
		if index < len(actual.routes) {
			observed = actual.routes[index]
		}
		routeResult := cloud.RouteReadyResult(routeStateFromObservation(observed))
		var routePlan []routeMutation
		node := desired.spec.Routes[index]
		switch node.Disposition {
		case cloud.GraphDelete:
			routePlan = buildRouteDeletionPlan(observed)
		case cloud.GraphPreserve:
			// An unresolved attached fragment may be observed, but it cannot
			// authorize reopening traffic through a disabled parent.
			if observed.pool != nil || len(observed.policies) != 0 {
				canRestoreTraffic = false
			}
		case cloud.GraphEnsure:
			if actual.loadBalancer != nil {
				if err := validateGraphLoadBalancer(actual.loadBalancer, route.spec.GatewayRequirements); err != nil {
					return nil, result, &cloud.GraphError{Identity: route.spec.Identity, Err: err}
				}
				if actual.listener != nil && actual.listener.ProtocolPort != route.spec.GatewayRequirements.ListenerPort {
					return nil, result, &cloud.GraphError{Identity: route.spec.Identity, Err: fmt.Errorf("%w: route listener port differs from its binding", cloud.ErrOwnershipConflict)}
				}
				if actual.floatingIP != nil && route.spec.GatewayRequirements.ExternalNetworkID != "" && actual.floatingIP.FloatingNetworkID != route.spec.GatewayRequirements.ExternalNetworkID {
					return nil, result, &cloud.GraphError{Identity: route.spec.Identity, Err: fmt.Errorf("%w: route Floating IP network differs from its binding", cloud.ErrOwnershipConflict)}
				}
			}
			if route.spec.Gateway.LoadBalancerID != "" && actual.state.LoadBalancerID != "" && route.spec.Gateway.LoadBalancerID != actual.state.LoadBalancerID ||
				route.spec.Gateway.ListenerID != "" && actual.state.ListenerID != "" && route.spec.Gateway.ListenerID != actual.state.ListenerID {
				return nil, result, &cloud.GraphError{Identity: route.spec.Identity, Err: fmt.Errorf("%w: route Gateway IDs differ from the observed graph", cloud.ErrOwnershipConflict)}
			}
			route.spec.Gateway = actual.state
			routePlan, routeResult.State, err = buildRouteEnsurePlan(route, observed)
			if err != nil {
				return nil, result, &cloud.GraphError{Identity: route.spec.Identity, Err: err}
			}
			if actual.listener == nil || actual.loadBalancer == nil {
				// The desired route has been validated, but its resources cannot
				// be created until both prerequisites have been observed.
				routePlan = nil
			}
			if !gatewayReady {
				routeResult.Outcome = p.progressingOutcome("Waiting for the Gateway infrastructure")
			}
		}
		if len(routePlan) != 0 {
			routeResult.Outcome = p.progressingOutcome(routePlan[0].message())
		}
		for _, step := range routePlan {
			mutation := graphMutation{kind: graphRouteMutation, routeIndex: index, route: step, message: step.message()}
			if node.Disposition == cloud.GraphDelete {
				deletions = append(deletions, mutation)
			} else {
				ensures = append(ensures, mutation)
			}
		}
		result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: route.spec.Identity, Result: routeResult})
		if routeResult.Outcome.State == cloud.OutcomeProgressing {
			result.Outcome = routeResult.Outcome
		}
	}
	plan = append(plan, deletions...)
	plan = append(plan, ensures...)
	if desired.spec.Gateway.Disposition == cloud.GraphEnsure && actual.loadBalancer != nil && actual.listener != nil && canRestoreTraffic {
		if !actual.listener.AdminStateUp {
			plan = append(plan, graphMutation{kind: graphEnableListener, message: "Enabled Octavia listener after validating the Gateway graph"})
		}
		if !actual.loadBalancer.AdminStateUp {
			plan = append(plan, graphMutation{kind: graphEnableLoadBalancer, message: "Enabled Octavia load balancer after validating the Gateway graph"})
		}
	}
	if len(plan) != 0 {
		result.Outcome = p.progressingOutcome(plan[0].message)
	} else if result.Gateway.Outcome.State == cloud.OutcomeProgressing && desired.spec.Gateway.Disposition == cloud.GraphEnsure {
		result.Outcome = result.Gateway.Outcome
	}
	return plan, result, nil
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
	if networkID != "" && actual.floatingIP == nil {
		plan = append(plan, graphMutation{kind: graphCreateFloatingIP, message: "Allocated Floating IP"})
	}
	if networkID == "" && actual.floatingIP != nil {
		plan = append(plan, graphMutation{kind: graphDeleteFloatingIP, message: "Removed a no-longer-configured Floating IP"})
	}
	if actual.listener == nil {
		plan = append(plan, graphMutation{kind: graphCreateListener, message: "Created Octavia listener"})
	}
	return plan, nil
}

func gatewayGraphDeletionPlan(actual observedGatewayGraph) []graphMutation {
	plan := make([]graphMutation, 0)
	for _, step := range actual.deletion.orderedSteps() {
		plan = append(plan, graphMutation{kind: graphDeleteChild, deletion: step, message: fmt.Sprintf("Deleting controller-owned %s", step.resource)})
	}
	if actual.floatingIP != nil {
		plan = append(plan, graphMutation{kind: graphDeleteFloatingIP, message: "Deleting controller-owned Floating IP"})
	}
	if actual.loadBalancer != nil {
		plan = append(plan, graphMutation{kind: graphDeleteLoadBalancer, message: "Deleting Octavia load balancer"})
	}
	return plan
}

func routeStateFromObservation(actual observedRouteGraph) cloud.RouteState {
	state := cloud.RouteState{}
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
		created, createErr := p.octavia.CreateLoadBalancer(ctx, loadbalancers.CreateOpts{
			Name: resourceName(desired.spec.Identity, roleLoadBalancer), Description: desired.identity.Description(roleLoadBalancer),
			VipSubnetID: spec.VIPSubnetID, Provider: spec.Provider, FlavorID: spec.FlavorID, AvailabilityZone: spec.AvailabilityZone,
			AdminStateUp: boolPointer(true), Tags: tags,
		})
		err = createErr
		if err == nil {
			err = p.validateLoadBalancerProject(created.ID, created.ProjectID)
		}
	case graphCreateFloatingIP:
		created, createErr := p.neutron.CreateFloatingIP(ctx, floatingips.CreateOpts{
			Description: desired.identity.GatewayDescription(roleFloatingIP), FloatingNetworkID: spec.ExternalNetworkID, PortID: actual.state.VIPPortID,
		})
		if createErr != nil {
			return fmt.Errorf("create Floating IP: %w", createErr)
		}
		return p.validateFloatingIPProject(created.ID, created.ProjectID, created.TenantID)
	case graphDeleteFloatingIP:
		err = p.neutron.DeleteFloatingIP(ctx, actual.floatingIP.ID)
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("delete Floating IP: %w", err)
		}
		return nil
	case graphCreateListener:
		tags, tagErr := desired.identity.GatewayTags(roleListener)
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreateListener(ctx, listeners.CreateOpts{
			LoadbalancerID: actual.state.LoadBalancerID, Protocol: listeners.ProtocolHTTP, ProtocolPort: spec.ListenerPort,
			Name: spec.ListenerName, Description: desired.identity.Description(roleListener), AdminStateUp: boolPointer(true), Tags: tags,
		})
	case graphRouteMutation:
		route := desired.routes[step.routeIndex]
		route.spec.Gateway = actual.state
		return p.executeRouteMutation(ctx, route, step.route)
	case graphEnableListener:
		return p.enableListener(ctx, actual.state.ListenerID)
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
