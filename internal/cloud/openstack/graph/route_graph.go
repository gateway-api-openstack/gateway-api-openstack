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
	"net/http"
	"net/netip"
	"sort"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

const (
	routeMonitorDelay          = 10
	routeMonitorTimeout        = 5
	routeMonitorMaxRetries     = 3
	routeMonitorMaxRetriesDown = 3
	routeMonitorExpectedCodes  = "200-399"
)

type observedRoutePolicy struct {
	policy l7policies.L7Policy
	role   string
	rules  []observedRouteRule
}

type observedRouteRule struct {
	rule l7policies.Rule
	role string
}

type observedRouteGraph struct {
	pool     *pools.Pool
	members  []pools.Member
	monitor  *monitors.Monitor
	policies []observedRoutePolicy
}

// observeGatewayRoutes reads every child once and assigns it to an exact route
// identity. An unaccounted child blocks the entire graph before any mutation.
func (p *Provider) observeGatewayRoutes(ctx context.Context, identities []Identity, gateway cloud.GatewayState) ([]observedRouteGraph, error) {
	graphs := make([]observedRouteGraph, len(identities))
	poolItems, err := p.octavia.ListPools(ctx, pools.ListOpts{
		LoadbalancerID: gateway.LoadBalancerID,
		ProjectID:      p.projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("list pools beneath Gateway: %w", err)
	}
	sort.Slice(poolItems, func(i, j int) bool { return poolItems[i].ID < poolItems[j].ID })
	for index := range poolItems {
		pool := &poolItems[index]
		routeIndex, err := routeOwnerIndex(identities, pool.Tags, rolePool)
		if err != nil {
			return nil, fmt.Errorf("validate pool %s: %w", pool.ID, err)
		}
		identity := identities[routeIndex]
		if err := p.validateGatewayDeletionPool(identity, *pool, gateway.LoadBalancerID); err != nil {
			return nil, &cloud.GraphError{Identity: identity.Value(), Err: err}
		}
		graph := &graphs[routeIndex]
		if graph.pool != nil {
			return nil, &cloud.GraphError{Identity: identity.Value(), Err: fmt.Errorf("%w: found multiple pools for one HTTPRoute", cloud.ErrOwnershipConflict)}
		}
		graph.pool = pool
		if err := p.observeRoutePoolChildren(ctx, identity, graph); err != nil {
			return nil, &cloud.GraphError{Identity: identity.Value(), Err: err}
		}
	}
	if gateway.ListenerID == "" {
		return graphs, nil
	}
	items, err := p.octavia.ListPolicies(ctx, l7policies.ListOpts{ListenerID: gateway.ListenerID, ProjectID: p.projectID})
	if err != nil {
		return nil, fmt.Errorf("list Gateway L7 policies: %w", err)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Position == items[j].Position {
			return items[i].ID < items[j].ID
		}
		return items[i].Position < items[j].Position
	})
	for _, item := range items {
		routeIndex, err := routeOwnerIndex(identities, item.Tags, rolePolicyExact, rolePolicyPrefix)
		if err != nil {
			return nil, fmt.Errorf("validate L7 policy %s: %w", item.ID, err)
		}
		observed, err := p.observeRoutePolicy(ctx, identities[routeIndex], item, gateway.ListenerID)
		if err != nil {
			return nil, &cloud.GraphError{Identity: identities[routeIndex].Value(), Err: err}
		}
		graph := &graphs[routeIndex]
		for _, policy := range graph.policies {
			if policy.role == observed.role {
				return nil, &cloud.GraphError{Identity: identities[routeIndex].Value(), Err: fmt.Errorf("%w: duplicate %s policies %s and %s", cloud.ErrOwnershipConflict, observed.role, policy.policy.ID, item.ID)}
			}
		}
		graph.policies = append(graph.policies, observed)
	}
	return graphs, nil
}

func routeOwnerIndex(identities []Identity, tags []string, roles ...string) (int, error) {
	for index, identity := range identities {
		for _, role := range roles {
			if identity.MatchesRoute(tags, role) {
				return index, nil
			}
		}
	}
	return -1, fmt.Errorf("%w: Gateway contains an unaccounted or foreign route resource", cloud.ErrOwnershipConflict)
}

func (p *Provider) observeRoutePoolChildren(ctx context.Context, identity Identity, graph *observedRouteGraph) error {
	var err error
	graph.members, err = p.octavia.ListMembers(ctx, graph.pool.ID, pools.ListMembersOpts{
		ProjectID: p.projectID,
	})
	if err != nil {
		return fmt.Errorf("list route pool members: %w", err)
	}
	sort.Slice(graph.members, func(i, j int) bool {
		if graph.members[i].Address == graph.members[j].Address {
			if graph.members[i].ProtocolPort == graph.members[j].ProtocolPort {
				return graph.members[i].ID < graph.members[j].ID
			}
			return graph.members[i].ProtocolPort < graph.members[j].ProtocolPort
		}
		return graph.members[i].Address < graph.members[j].Address
	})
	memberKeys := make(map[string]string, len(graph.members))
	for index := range graph.members {
		member := &graph.members[index]
		if err := p.validateOptionalProject("member", member.ID, member.ProjectID); err != nil {
			return err
		}
		if !identity.MatchesRoute(member.Tags, roleMember) {
			return fmt.Errorf("%w: pool %s contains member %s not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict, graph.pool.ID, member.ID)
		}
		if member.PoolID != "" && member.PoolID != graph.pool.ID {
			return fmt.Errorf("%w: member %s reports pool %s instead of %s", cloud.ErrOwnershipConflict, member.ID, member.PoolID, graph.pool.ID)
		}
		address, err := netip.ParseAddr(member.Address)
		if err != nil || member.ProtocolPort < 1 || member.ProtocolPort > 65535 {
			return fmt.Errorf("%w: member %s has invalid address or port %s:%d", cloud.ErrOwnershipConflict, member.ID, member.Address, member.ProtocolPort)
		}
		member.Address = address.String()
		key := memberKey(member.Address, member.ProtocolPort)
		if existingID := memberKeys[key]; existingID != "" {
			return fmt.Errorf("%w: pool %s has duplicate members %s and %s for %s", cloud.ErrOwnershipConflict, graph.pool.ID, existingID, member.ID, key)
		}
		memberKeys[key] = member.ID
	}

	monitorItems, err := p.octavia.ListMonitors(ctx, monitors.ListOpts{
		PoolID:    graph.pool.ID,
		ProjectID: p.projectID,
	})
	if err != nil {
		return fmt.Errorf("list route health monitors: %w", err)
	}
	sort.Slice(monitorItems, func(i, j int) bool { return monitorItems[i].ID < monitorItems[j].ID })
	for index := range monitorItems {
		monitor := &monitorItems[index]
		if err := p.validateOptionalProject("health monitor", monitor.ID, monitor.ProjectID); err != nil {
			return err
		}
		if !identity.MatchesRoute(monitor.Tags, roleMonitor) {
			return fmt.Errorf("%w: pool %s contains health monitor %s not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict, graph.pool.ID, monitor.ID)
		}
		for _, parent := range monitor.Pools {
			if parent.ID != graph.pool.ID {
				return fmt.Errorf("%w: health monitor %s reports pool %s instead of %s", cloud.ErrOwnershipConflict, monitor.ID, parent.ID, graph.pool.ID)
			}
		}
		if graph.monitor != nil {
			return fmt.Errorf("%w: pool %s has multiple health monitors", cloud.ErrOwnershipConflict, graph.pool.ID)
		}
		graph.monitor = monitor
	}
	return nil
}

func (p *Provider) observeRoutePolicy(ctx context.Context, identity Identity, item l7policies.L7Policy, listenerID string) (observedRoutePolicy, error) {
	if err := p.validateOptionalProject("L7 policy", item.ID, item.ProjectID); err != nil {
		return observedRoutePolicy{}, err
	}
	role, err := routePolicyRole(identity, item.Tags)
	if err != nil {
		return observedRoutePolicy{}, fmt.Errorf("validate L7 policy %s: %w", item.ID, err)
	}
	if item.ListenerID != "" && item.ListenerID != listenerID {
		return observedRoutePolicy{}, fmt.Errorf("%w: L7 policy %s reports listener %s instead of %s", cloud.ErrOwnershipConflict, item.ID, item.ListenerID, listenerID)
	}
	ruleItems, err := p.octavia.ListRules(ctx, item.ID, l7policies.ListRulesOpts{ProjectID: p.projectID})
	if err != nil {
		return observedRoutePolicy{}, fmt.Errorf("list rules beneath L7 policy %s: %w", item.ID, err)
	}
	sort.Slice(ruleItems, func(i, j int) bool { return ruleItems[i].ID < ruleItems[j].ID })
	observedRules := make([]observedRouteRule, 0, len(ruleItems))
	ruleRoles := make(map[string]string, len(ruleItems))
	for _, rule := range ruleItems {
		if err := p.validateOptionalProject("L7 rule", rule.ID, rule.ProjectID); err != nil {
			return observedRoutePolicy{}, err
		}
		ruleRole, err := routeRuleRole(identity, rule.Tags)
		if err != nil {
			return observedRoutePolicy{}, fmt.Errorf("validate L7 rule %s beneath policy %s: %w", rule.ID, item.ID, err)
		}
		if existingID := ruleRoles[ruleRole]; existingID != "" {
			return observedRoutePolicy{}, fmt.Errorf("%w: policy %s has duplicate %s rules %s and %s", cloud.ErrOwnershipConflict, item.ID, ruleRole, existingID, rule.ID)
		}
		ruleRoles[ruleRole] = rule.ID
		observedRules = append(observedRules, observedRouteRule{rule: rule, role: ruleRole})
	}
	return observedRoutePolicy{policy: item, role: role, rules: observedRules}, nil
}

func routePolicyRole(identity Identity, tags []string) (string, error) {
	for _, role := range []string{rolePolicyExact, rolePolicyPrefix} {
		if identity.MatchesRoute(tags, role) {
			return role, nil
		}
		if identity.MatchesRouteDiscovery(tags, role) {
			return "", fmt.Errorf("%w: policy has an incomplete or stale identity", cloud.ErrOwnershipConflict)
		}
	}
	return "", fmt.Errorf("%w: policy is not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict)
}

func routeRuleRole(identity Identity, tags []string) (string, error) {
	for _, role := range []string{roleRulePath, roleRuleHost} {
		if identity.MatchesRoute(tags, role) {
			return role, nil
		}
		if identity.MatchesRouteDiscovery(tags, role) {
			return "", fmt.Errorf("%w: rule has an incomplete or stale identity", cloud.ErrOwnershipConflict)
		}
	}
	return "", fmt.Errorf("%w: rule is not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict)
}

type routeMutationKind uint8

const (
	routeMutationCreatePool routeMutationKind = iota
	routeMutationUpdatePool
	routeMutationDeleteMember
	routeMutationCreateMember
	routeMutationUpdateMember
	routeMutationCreateMonitor
	routeMutationUpdateMonitor
	routeMutationDisablePolicy
	routeMutationDeleteRule
	routeMutationDeletePolicy
	routeMutationCreatePolicy
	routeMutationUpdatePolicy
	routeMutationCreateRule
	routeMutationUpdateRule
	routeMutationEnablePolicy
	routeMutationDeleteMonitor
	routeMutationDeletePool
)

type routeMutation struct {
	kind     routeMutationKind
	id       string
	parentID string
	member   cloud.Member
	policy   desiredPolicy
	rule     desiredRule
}

func (s routeMutation) message() string {
	switch s.kind {
	case routeMutationCreatePool:
		return "Created Octavia pool"
	case routeMutationUpdatePool:
		return "Updated Octavia pool"
	case routeMutationDeleteMember:
		return "Deleted stale Octavia member"
	case routeMutationCreateMember:
		return "Created Octavia member"
	case routeMutationUpdateMember:
		return "Updated Octavia member"
	case routeMutationCreateMonitor:
		return "Created Octavia health monitor"
	case routeMutationUpdateMonitor:
		return "Updated Octavia health monitor"
	case routeMutationDisablePolicy:
		return "Disabled Octavia L7 policy before changing rules"
	case routeMutationDeleteRule:
		return "Deleted Octavia L7 rule"
	case routeMutationDeletePolicy:
		return "Deleted Octavia L7 policy"
	case routeMutationCreatePolicy:
		return "Created Octavia L7 policy"
	case routeMutationUpdatePolicy:
		return "Updated Octavia L7 policy"
	case routeMutationCreateRule:
		return "Created Octavia L7 rule"
	case routeMutationUpdateRule:
		return "Updated Octavia L7 rule"
	case routeMutationEnablePolicy:
		return "Enabled converged Octavia L7 policy"
	case routeMutationDeleteMonitor:
		return "Deleted Octavia health monitor"
	case routeMutationDeletePool:
		return "Deleted Octavia pool"
	default:
		return "Changed Octavia route graph"
	}
}

func buildRouteEnsurePlan(desired desiredRoute, actual observedRouteGraph) ([]routeMutation, cloud.RouteState, error) {
	nonPolicyPlan := make([]routeMutation, 0)
	state := cloud.RouteState{}
	poolID := ""
	if actual.pool == nil {
		nonPolicyPlan = append(nonPolicyPlan, routeMutation{kind: routeMutationCreatePool})
	} else {
		pool := actual.pool
		poolID = pool.ID
		state.PoolID = pool.ID
		if pool.Protocol != string(pools.ProtocolHTTP) {
			return nil, cloud.RouteState{}, fmt.Errorf("%w: pool %s has protocol %s", cloud.ErrOwnershipConflict, pool.ID, pool.Protocol)
		}
		if pool.LBMethod != string(pools.LBMethodRoundRobin) || !pool.AdminStateUp {
			nonPolicyPlan = append(nonPolicyPlan, routeMutation{kind: routeMutationUpdatePool, id: pool.ID})
		}

		desiredMembers := make(map[string]cloud.Member, len(desired.members))
		for _, member := range desired.members {
			desiredMembers[memberKey(member.Address, member.Port)] = member
		}
		for _, member := range actual.members {
			key := memberKey(member.Address, member.ProtocolPort)
			if _, exists := desiredMembers[key]; !exists || member.SubnetID != desired.spec.MemberSubnetID {
				nonPolicyPlan = append(nonPolicyPlan, routeMutation{kind: routeMutationDeleteMember, id: member.ID, parentID: pool.ID})
				continue
			}
			state.MemberIDs = append(state.MemberIDs, member.ID)
			delete(desiredMembers, key)
			if !member.AdminStateUp {
				nonPolicyPlan = append(nonPolicyPlan, routeMutation{
					kind:     routeMutationUpdateMember,
					id:       member.ID,
					parentID: pool.ID,
				})
			}
		}
		missingMemberKeys := make([]string, 0, len(desiredMembers))
		for key := range desiredMembers {
			missingMemberKeys = append(missingMemberKeys, key)
		}
		sort.Strings(missingMemberKeys)
		for _, key := range missingMemberKeys {
			nonPolicyPlan = append(nonPolicyPlan, routeMutation{kind: routeMutationCreateMember, parentID: pool.ID, member: desiredMembers[key]})
		}

		if actual.monitor == nil {
			nonPolicyPlan = append(nonPolicyPlan, routeMutation{kind: routeMutationCreateMonitor, parentID: pool.ID})
		} else {
			monitor := actual.monitor
			state.MonitorID = monitor.ID
			if monitor.Type != monitors.TypeHTTP {
				return nil, cloud.RouteState{}, fmt.Errorf("%w: health monitor %s has type %s", cloud.ErrOwnershipConflict, monitor.ID, monitor.Type)
			}
			if monitor.Delay != desired.healthMonitor.Delay || monitor.Timeout != desired.healthMonitor.Timeout ||
				monitor.MaxRetries != desired.healthMonitor.MaxRetries || monitor.MaxRetriesDown != desired.healthMonitor.MaxRetriesDown ||
				monitor.URLPath != desired.healthMonitor.Path || monitor.HTTPMethod != http.MethodGet ||
				monitor.ExpectedCodes != routeMonitorExpectedCodes || !monitor.AdminStateUp {
				nonPolicyPlan = append(nonPolicyPlan, routeMutation{kind: routeMutationUpdateMonitor, id: monitor.ID, parentID: pool.ID})
			}
		}
	}

	policyPlan, policyState, semanticDiff := buildRoutePolicyPlan(desired, actual.policies, poolID)
	state.L7PolicyIDs = policyState.L7PolicyIDs
	state.L7RuleIDs = policyState.L7RuleIDs
	if semanticDiff {
		disablePlan := make([]routeMutation, 0, len(actual.policies))
		for _, policy := range actual.policies {
			if policy.policy.AdminStateUp {
				disablePlan = append(disablePlan, routeMutation{kind: routeMutationDisablePolicy, id: policy.policy.ID})
			}
		}
		plan := append(disablePlan, nonPolicyPlan...)
		return append(plan, policyPlan...), state, nil
	}
	if len(nonPolicyPlan) != 0 {
		return nonPolicyPlan, state, nil
	}
	for _, wantedPolicy := range desired.policies {
		for _, policy := range actual.policies {
			if policy.role == wantedPolicy.role && !policy.policy.AdminStateUp {
				return []routeMutation{{kind: routeMutationEnablePolicy, id: policy.policy.ID}}, state, nil
			}
		}
	}
	return nil, state, nil
}

func buildRoutePolicyPlan(
	desired desiredRoute,
	actual []observedRoutePolicy,
	poolID string,
) ([]routeMutation, cloud.RouteState, bool) {
	plan := make([]routeMutation, 0)
	state := cloud.RouteState{}
	semanticDiff := false
	desiredPolicies := make(map[string]desiredPolicy, len(desired.policies))
	for _, policy := range desired.policies {
		desiredPolicies[policy.role] = policy
	}
	for _, policy := range actual {
		if _, wanted := desiredPolicies[policy.role]; wanted {
			continue
		}
		semanticDiff = true
		for _, rule := range policy.rules {
			plan = append(plan, routeMutation{kind: routeMutationDeleteRule, id: rule.rule.ID, parentID: policy.policy.ID})
		}
		plan = append(plan, routeMutation{kind: routeMutationDeletePolicy, id: policy.policy.ID})
	}

	actualPolicies := make(map[string]observedRoutePolicy, len(actual))
	for _, policy := range actual {
		actualPolicies[policy.role] = policy
	}
	for _, wantedPolicy := range desired.policies {
		observedPolicy, exists := actualPolicies[wantedPolicy.role]
		if !exists {
			semanticDiff = true
			plan = append(plan, routeMutation{kind: routeMutationCreatePolicy, parentID: poolID, policy: wantedPolicy})
			continue
		}
		policy := observedPolicy.policy
		state.L7PolicyIDs = append(state.L7PolicyIDs, policy.ID)
		if policy.Action != string(l7policies.ActionRedirectToPool) || policy.RedirectPoolID != poolID ||
			policy.Position != wantedPolicy.position {
			semanticDiff = true
			plan = append(plan, routeMutation{kind: routeMutationUpdatePolicy, id: policy.ID, parentID: poolID, policy: wantedPolicy})
		}

		wantedRules := rulesFor(wantedPolicy, desired.spec.Hostname)
		actualRules := make(map[string]observedRouteRule, len(observedPolicy.rules))
		for _, rule := range observedPolicy.rules {
			actualRules[rule.role] = rule
		}
		for _, wantedRule := range wantedRules {
			observedRule, exists := actualRules[wantedRule.role]
			if !exists {
				semanticDiff = true
				plan = append(plan, routeMutation{kind: routeMutationCreateRule, parentID: policy.ID, rule: wantedRule})
				continue
			}
			rule := observedRule.rule
			state.L7RuleIDs = append(state.L7RuleIDs, rule.ID)
			if rule.RuleType != string(wantedRule.ruleType) || rule.CompareType != string(wantedRule.compareType) ||
				rule.Value != wantedRule.value || rule.Invert || !rule.AdminStateUp {
				semanticDiff = true
				plan = append(plan, routeMutation{kind: routeMutationUpdateRule, id: rule.ID, parentID: policy.ID, rule: wantedRule})
			}
			delete(actualRules, wantedRule.role)
		}
		staleRules := make([]observedRouteRule, 0, len(actualRules))
		for _, rule := range actualRules {
			staleRules = append(staleRules, rule)
		}
		sort.Slice(staleRules, func(i, j int) bool { return staleRules[i].rule.ID < staleRules[j].rule.ID })
		for _, rule := range staleRules {
			semanticDiff = true
			plan = append(plan, routeMutation{kind: routeMutationDeleteRule, id: rule.rule.ID, parentID: policy.ID})
		}
	}
	return plan, state, semanticDiff
}

func buildRouteDeletionPlan(actual observedRouteGraph) []routeMutation {
	plan := make([]routeMutation, 0)
	for _, policy := range actual.policies {
		if policy.policy.AdminStateUp {
			plan = append(plan, routeMutation{kind: routeMutationDisablePolicy, id: policy.policy.ID})
		}
	}
	for _, policy := range actual.policies {
		for _, rule := range policy.rules {
			plan = append(plan, routeMutation{kind: routeMutationDeleteRule, id: rule.rule.ID, parentID: policy.policy.ID})
		}
	}
	for _, policy := range actual.policies {
		plan = append(plan, routeMutation{kind: routeMutationDeletePolicy, id: policy.policy.ID})
	}
	if actual.monitor != nil {
		plan = append(plan, routeMutation{kind: routeMutationDeleteMonitor, id: actual.monitor.ID, parentID: actual.pool.ID})
	}
	for _, member := range actual.members {
		plan = append(plan, routeMutation{kind: routeMutationDeleteMember, id: member.ID, parentID: actual.pool.ID})
	}
	if actual.pool != nil {
		plan = append(plan, routeMutation{kind: routeMutationDeletePool, id: actual.pool.ID})
	}
	return plan
}

func (p *Provider) executeRouteMutation(ctx context.Context, desired desiredRoute, step routeMutation) error {
	var err error
	switch step.kind {
	case routeMutationCreatePool:
		tags, tagErr := desired.identity.RouteTags(rolePool)
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreatePool(ctx, pools.CreateOpts{
			LBMethod:       pools.LBMethodRoundRobin,
			Protocol:       pools.ProtocolHTTP,
			LoadbalancerID: desired.spec.Gateway.LoadBalancerID,
			Name:           resourceName(desired.spec.Identity, rolePool),
			Description:    desired.identity.Description(rolePool),
			AdminStateUp:   boolPointer(true),
			Tags:           tags,
		})
	case routeMutationUpdatePool:
		_, err = p.octavia.UpdatePool(ctx, step.id, pools.UpdateOpts{
			LBMethod:     pools.LBMethodRoundRobin,
			AdminStateUp: boolPointer(true),
		})
	case routeMutationDeleteMember:
		err = p.octavia.DeleteMember(ctx, step.parentID, step.id)
	case routeMutationCreateMember:
		tags, tagErr := desired.identity.RouteTags(roleMember)
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreateMember(ctx, step.parentID, pools.CreateMemberOpts{
			Address:      step.member.Address,
			ProtocolPort: step.member.Port,
			Name:         resourceName(desired.spec.Identity, roleMember),
			SubnetID:     desired.spec.MemberSubnetID,
			AdminStateUp: boolPointer(true),
			Tags:         tags,
		})
	case routeMutationUpdateMember:
		_, err = p.octavia.UpdateMember(ctx, step.parentID, step.id, pools.UpdateMemberOpts{
			AdminStateUp: boolPointer(true),
		})
	case routeMutationCreateMonitor:
		tags, tagErr := desired.identity.RouteTags(roleMonitor)
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreateMonitor(ctx, monitors.CreateOpts{
			PoolID:         step.parentID,
			Type:           monitors.TypeHTTP,
			Delay:          desired.healthMonitor.Delay,
			Timeout:        desired.healthMonitor.Timeout,
			MaxRetries:     desired.healthMonitor.MaxRetries,
			MaxRetriesDown: desired.healthMonitor.MaxRetriesDown,
			URLPath:        desired.healthMonitor.Path,
			HTTPMethod:     http.MethodGet,
			ExpectedCodes:  routeMonitorExpectedCodes,
			Name:           resourceName(desired.spec.Identity, roleMonitor),
			AdminStateUp:   boolPointer(true),
			Tags:           tags,
		})
	case routeMutationUpdateMonitor:
		_, err = p.octavia.UpdateMonitor(ctx, step.id, monitors.UpdateOpts{
			Delay:          desired.healthMonitor.Delay,
			Timeout:        desired.healthMonitor.Timeout,
			MaxRetries:     desired.healthMonitor.MaxRetries,
			MaxRetriesDown: desired.healthMonitor.MaxRetriesDown,
			URLPath:        desired.healthMonitor.Path,
			HTTPMethod:     http.MethodGet,
			ExpectedCodes:  routeMonitorExpectedCodes,
			AdminStateUp:   boolPointer(true),
		})
	case routeMutationDisablePolicy:
		_, err = p.octavia.UpdatePolicy(ctx, step.id, l7policies.UpdateOpts{
			AdminStateUp: boolPointer(false),
		})
	case routeMutationDeleteRule:
		err = p.octavia.DeleteRule(ctx, step.parentID, step.id)
	case routeMutationDeletePolicy:
		err = p.octavia.DeletePolicy(ctx, step.id)
	case routeMutationCreatePolicy:
		tags, tagErr := desired.identity.RouteTags(step.policy.role)
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreatePolicy(ctx, l7policies.CreateOpts{
			Name:           resourceName(desired.spec.Identity, step.policy.role),
			ListenerID:     desired.spec.Gateway.ListenerID,
			Action:         l7policies.ActionRedirectToPool,
			Position:       step.policy.position,
			Description:    desired.identity.Description(step.policy.role),
			RedirectPoolID: step.parentID,
			AdminStateUp:   boolPointer(false),
			Tags:           tags,
		})
	case routeMutationUpdatePolicy:
		_, err = p.octavia.UpdatePolicy(ctx, step.id, l7policies.UpdateOpts{
			Action:         l7policies.ActionRedirectToPool,
			Position:       step.policy.position,
			RedirectPoolID: &step.parentID,
			AdminStateUp:   boolPointer(false),
		})
	case routeMutationCreateRule:
		tags, tagErr := desired.identity.RouteTags(step.rule.role)
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreateRule(ctx, step.parentID, l7policies.CreateRuleOpts{
			RuleType:     step.rule.ruleType,
			CompareType:  step.rule.compareType,
			Value:        step.rule.value,
			AdminStateUp: boolPointer(true),
			Tags:         tags,
		})
	case routeMutationUpdateRule:
		_, err = p.octavia.UpdateRule(ctx, step.parentID, step.id, l7policies.UpdateRuleOpts{
			RuleType:     step.rule.ruleType,
			CompareType:  step.rule.compareType,
			Value:        step.rule.value,
			Invert:       boolPointer(false),
			AdminStateUp: boolPointer(true),
		})
	case routeMutationEnablePolicy:
		_, err = p.octavia.UpdatePolicy(ctx, step.id, l7policies.UpdateOpts{
			AdminStateUp: boolPointer(true),
		})
	case routeMutationDeleteMonitor:
		err = p.octavia.DeleteMonitor(ctx, step.id)
	case routeMutationDeletePool:
		err = p.octavia.DeletePool(ctx, step.id)
	default:
		return fmt.Errorf("unsupported route mutation %d", step.kind)
	}
	if err != nil && isNotFound(err) && isRouteDeletion(step.kind) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", step.message(), classifyOctaviaMutationError(err))
	}
	return nil
}

func isRouteDeletion(kind routeMutationKind) bool {
	switch kind {
	case routeMutationDeleteMember, routeMutationDeleteRule, routeMutationDeletePolicy,
		routeMutationDeleteMonitor, routeMutationDeletePool:
		return true
	default:
		return false
	}
}
