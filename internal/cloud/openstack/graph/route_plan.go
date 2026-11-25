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
	"fmt"
	"net/http"
	"sort"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

func buildRouteEnsurePlan(desired desiredRoute, actual observedRouteGraph) ([]routeMutation, cloud.RouteRuleState, error) {
	nonPolicyPlan, state, err := buildRoutePoolPlan(desired, actual)
	if err != nil {
		return nil, cloud.RouteRuleState{}, err
	}
	policyPlan, policyState, semanticDiff := buildRoutePolicyPlan(desired, actual.policies, state.PoolID)
	state.L7PolicyIDs = policyState.L7PolicyIDs
	state.L7RuleIDs = policyState.L7RuleIDs
	if semanticDiff {
		changedPolicies := make(map[string]struct{})
		for _, step := range policyPlan {
			switch step.kind {
			case routeMutationUpdatePolicy, routeMutationDeletePolicy:
				changedPolicies[step.id] = struct{}{}
			case routeMutationCreateRule, routeMutationUpdateRule, routeMutationDeleteRule:
				changedPolicies[step.parentID] = struct{}{}
			}
		}
		disablePlan := make([]routeMutation, 0, len(actual.policies))
		for _, policy := range actual.policies {
			if _, changed := changedPolicies[policy.policy.ID]; changed && policy.policy.AdminStateUp {
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
			if policy.key == wantedPolicy.key && !policy.policy.AdminStateUp {
				return []routeMutation{{kind: routeMutationEnablePolicy, id: policy.policy.ID}}, state, nil
			}
		}
	}
	return nil, state, nil
}

func buildRoutePoolPlan(desired desiredRoute, actual observedRouteGraph) ([]routeMutation, cloud.RouteRuleState, error) {
	state := cloud.RouteRuleState{Index: desired.rule.Index}
	if actual.pool == nil {
		return []routeMutation{{kind: routeMutationCreatePool}}, state, nil
	}
	pool := actual.pool
	if pool.Protocol != string(pools.ProtocolHTTP) {
		return nil, cloud.RouteRuleState{}, fmt.Errorf("%w: pool %s has protocol %s", cloud.ErrOwnershipConflict, pool.ID, pool.Protocol)
	}
	state.PoolID = pool.ID
	var plan []routeMutation
	if pool.LBMethod != string(pools.LBMethodRoundRobin) || !pool.AdminStateUp {
		plan = append(plan, routeMutation{kind: routeMutationUpdatePool, id: pool.ID})
	}
	memberPlan, memberIDs := buildRouteMemberPlan(desired, actual)
	state.MemberIDs = memberIDs
	plan = append(plan, memberPlan...)
	monitorPlan, monitorID, err := buildRouteMonitorPlan(desired, actual)
	if err != nil {
		return nil, cloud.RouteRuleState{}, err
	}
	state.MonitorID = monitorID
	return append(plan, monitorPlan...), state, nil
}

// The pool must already exist before member changes can be planned.
func buildRouteMemberPlan(desired desiredRoute, actual observedRouteGraph) ([]routeMutation, []string) {
	wanted := make(map[string]cloud.Member, len(desired.members))
	for _, member := range desired.members {
		wanted[memberKey(member.Address, member.Port)] = member
	}
	var plan []routeMutation
	var memberIDs []string
	for _, member := range actual.members {
		key := memberKey(member.Address, member.ProtocolPort)
		desiredMember, exists := wanted[key]
		if !exists || member.SubnetID != desired.spec.MemberSubnetID {
			plan = append(plan, routeMutation{kind: routeMutationDeleteMember, id: member.ID, parentID: actual.pool.ID})
			continue
		}
		memberIDs = append(memberIDs, member.ID)
		delete(wanted, key)
		weight := desiredMemberWeight(desired.spec.MemberMode, desiredMember)
		if !member.AdminStateUp || weight != nil && member.Weight != *weight {
			plan = append(plan, routeMutation{kind: routeMutationUpdateMember, id: member.ID, parentID: actual.pool.ID, member: desiredMember})
		}
	}
	missing := make([]string, 0, len(wanted))
	for key := range wanted {
		missing = append(missing, key)
	}
	sort.Strings(missing)
	for _, key := range missing {
		plan = append(plan, routeMutation{kind: routeMutationCreateMember, parentID: actual.pool.ID, member: wanted[key]})
	}
	return plan, memberIDs
}

func buildRouteMonitorPlan(desired desiredRoute, actual observedRouteGraph) ([]routeMutation, string, error) {
	if actual.monitor == nil {
		return []routeMutation{{kind: routeMutationCreateMonitor, parentID: actual.pool.ID}}, "", nil
	}
	monitor := actual.monitor
	if monitor.Type != monitors.TypeHTTP {
		return nil, "", fmt.Errorf("%w: health monitor %s has type %s", cloud.ErrOwnershipConflict, monitor.ID, monitor.Type)
	}
	if monitor.Delay != desired.healthMonitor.Delay || monitor.Timeout != desired.healthMonitor.Timeout ||
		monitor.MaxRetries != desired.healthMonitor.MaxRetries || monitor.MaxRetriesDown != desired.healthMonitor.MaxRetriesDown ||
		monitor.URLPath != desired.healthMonitor.Path || monitor.HTTPMethod != http.MethodGet ||
		monitor.ExpectedCodes != routeMonitorExpectedCodes || !monitor.AdminStateUp {
		return []routeMutation{{kind: routeMutationUpdateMonitor, id: monitor.ID, parentID: actual.pool.ID}}, monitor.ID, nil
	}
	return nil, monitor.ID, nil
}

func buildRoutePolicyPlan(
	desired desiredRoute,
	actual []observedRoutePolicy,
	poolID string,
) ([]routeMutation, cloud.RouteRuleState, bool) {
	plan := make([]routeMutation, 0)
	state := cloud.RouteRuleState{Index: desired.rule.Index}
	semanticDiff := false
	desiredPolicies := make(map[cloud.ResourceKey]desiredPolicy, len(desired.policies))
	for _, policy := range desired.policies {
		desiredPolicies[policy.key] = policy
	}
	for _, policy := range actual {
		if _, wanted := desiredPolicies[policy.key]; wanted {
			continue
		}
		semanticDiff = true
		for _, rule := range policy.rules {
			plan = append(plan, routeMutation{kind: routeMutationDeleteRule, id: rule.rule.ID, parentID: policy.policy.ID})
		}
		plan = append(plan, routeMutation{kind: routeMutationDeletePolicy, id: policy.policy.ID})
	}

	actualPolicies := make(map[cloud.ResourceKey]observedRoutePolicy, len(actual))
	for _, policy := range actual {
		actualPolicies[policy.key] = policy
	}
	for _, wantedPolicy := range desired.policies {
		observedPolicy, exists := actualPolicies[wantedPolicy.key]
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

		wantedRules := rulesFor(wantedPolicy)
		actualRules := make(map[cloud.ResourceKey]observedRouteRule, len(observedPolicy.rules))
		for _, rule := range observedPolicy.rules {
			actualRules[rule.key] = rule
		}
		for _, wantedRule := range wantedRules {
			observedRule, exists := actualRules[wantedRule.key]
			if !exists {
				semanticDiff = true
				plan = append(plan, routeMutation{kind: routeMutationCreateRule, parentID: policy.ID, rule: wantedRule})
				continue
			}
			rule := observedRule.rule
			state.L7RuleIDs = append(state.L7RuleIDs, rule.ID)
			if rule.RuleType != string(wantedRule.ruleType) || rule.CompareType != string(wantedRule.compareType) ||
				rule.Key != wantedRule.header || rule.Value != wantedRule.value || rule.Invert || !rule.AdminStateUp {
				semanticDiff = true
				plan = append(plan, routeMutation{kind: routeMutationUpdateRule, id: rule.ID, parentID: policy.ID, rule: wantedRule})
			}
			delete(actualRules, wantedRule.key)
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
