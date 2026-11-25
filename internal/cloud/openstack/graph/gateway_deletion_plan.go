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
	"sort"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

type gatewayDeletionResource string

const (
	gatewayDeletionDisablePolicy gatewayDeletionResource = "active L7 policy"
	gatewayDeletionL7Rule        gatewayDeletionResource = "L7 rule"
	gatewayDeletionL7Policy      gatewayDeletionResource = "L7 policy"
	gatewayDeletionListener      gatewayDeletionResource = "listener"
	gatewayDeletionMonitor       gatewayDeletionResource = "health monitor"
	gatewayDeletionMember        gatewayDeletionResource = "member"
	gatewayDeletionPool          gatewayDeletionResource = "pool"
)

type gatewayDeletionStep struct {
	resource   gatewayDeletionResource
	resourceID string
	parentID   string
}

type gatewayListenerDeletionPlan []gatewayDeletionStep
type gatewayRouteDeletionPlan []gatewayDeletionStep
type gatewayPoolDeletionPlan []gatewayDeletionStep

// gatewayDeletionPlan separates the load balancer graph into dependency
// subplans. No step may run until all three subplans have been validated.
type gatewayDeletionPlan struct {
	listeners gatewayListenerDeletionPlan
	routes    gatewayRouteDeletionPlan
	pools     gatewayPoolDeletionPlan
}

// orderedSteps flattens the validated subplans in reverse dependency order.
func (p gatewayDeletionPlan) orderedSteps() []gatewayDeletionStep {
	steps := make([]gatewayDeletionStep, 0, len(p.routes)+len(p.pools)+len(p.listeners))
	steps = append(steps, p.routes...)
	steps = append(steps, p.pools...)
	steps = append(steps, p.listeners...)
	return steps
}

// buildGatewayDeletionPlan validates the complete load balancer graph before
// returning any work. Callers must not execute a deletion step when this
// method returns an error.
func (p *Provider) buildGatewayDeletionPlan(ctx context.Context, identity Identity, loadBalancerID string) (gatewayDeletionPlan, error) {
	listeners, err := p.findManagedGatewayListeners(ctx, identity, loadBalancerID)
	if err != nil {
		return gatewayDeletionPlan{}, err
	}
	state := cloud.GatewayState{LoadBalancerID: loadBalancerID}
	for key, listener := range listeners {
		state.Listeners = append(state.Listeners, cloud.ListenerState{Key: key, ID: listener.ID})
	}
	sort.Slice(state.Listeners, func(i, j int) bool { return state.Listeners[i].Key.String() < state.Listeners[j].Key.String() })
	routes, err := p.observeOwnedGatewayRoutes(ctx, identity, state)
	if err != nil {
		return gatewayDeletionPlan{}, err
	}
	plan := gatewayDeletionPlan{}
	for _, listener := range state.Listeners {
		plan.listeners = append(plan.listeners, gatewayDeletionStep{resource: gatewayDeletionListener, resourceID: listener.ID})
	}
	uids := make([]string, 0, len(routes))
	for uid := range routes {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		indices := make([]int, 0, len(routes[uid]))
		for index := range routes[uid] {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			for _, mutation := range buildRouteDeletionPlan(*routes[uid][index]) {
				step := gatewayDeletionStep{resourceID: mutation.id, parentID: mutation.parentID}
				switch mutation.kind {
				case routeMutationDeleteRule:
					step.resource = gatewayDeletionL7Rule
				case routeMutationDeletePolicy:
					step.resource = gatewayDeletionL7Policy
				case routeMutationDeleteMonitor:
					step.resource = gatewayDeletionMonitor
				case routeMutationDeleteMember:
					step.resource = gatewayDeletionMember
				case routeMutationDeletePool:
					step.resource = gatewayDeletionPool
				case routeMutationDisablePolicy:
					step.resource = gatewayDeletionDisablePolicy
				default:
					return gatewayDeletionPlan{}, fmt.Errorf("unsupported deletion mutation %d", mutation.kind)
				}
				if step.resource == gatewayDeletionL7Rule || step.resource == gatewayDeletionL7Policy || step.resource == gatewayDeletionDisablePolicy {
					plan.routes = append(plan.routes, step)
				} else {
					plan.pools = append(plan.pools, step)
				}
			}
		}
	}
	return plan, nil
}

func (p *Provider) validateGatewayDeletionPool(identity Identity, pool pools.Pool, loadBalancerID string) error {
	if err := p.validateOptionalProject("pool", pool.ID, pool.ProjectID); err != nil {
		return err
	}
	if !identity.MatchesGateway(pool.Tags, rolePool) {
		return fmt.Errorf("%w: pool %s", cloud.ErrOwnershipConflict, pool.ID)
	}
	for _, parent := range pool.Loadbalancers {
		if parent.ID != loadBalancerID {
			return fmt.Errorf("%w: pool %s reports load balancer %s instead of %s", cloud.ErrOwnershipConflict, pool.ID, parent.ID, loadBalancerID)
		}
	}
	return nil
}
