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

	openstackidentity "github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/identity"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

const routeMonitorExpectedCodes = "200-399"

type observedRoutePolicy struct {
	policy l7policies.L7Policy
	role   string
	key    cloud.ResourceKey
	rules  []observedRouteRule
}

type observedRouteRule struct {
	rule l7policies.Rule
	role string
	key  cloud.ResourceKey
}

type observedRouteGraph struct {
	index    int
	pool     *pools.Pool
	members  []pools.Member
	monitor  *monitors.Monitor
	policies []observedRoutePolicy
}

// observeGatewayRoutes reads every child once and assigns it to an exact route
// identity. An unaccounted child blocks the entire graph before any mutation.
type observedRoutes map[string]map[int]*observedRouteGraph

func (graphs observedRoutes) rule(uid string, index int) *observedRouteGraph {
	if graphs[uid] == nil {
		graphs[uid] = make(map[int]*observedRouteGraph)
	}
	if graphs[uid][index] == nil {
		graphs[uid][index] = &observedRouteGraph{index: index}
	}
	return graphs[uid][index]
}

type routeOwnerResolver func([]string, ...string) (Identity, error)

func (p *Provider) observeGatewayRoutes(ctx context.Context, identities []Identity, gateway cloud.GatewayState) (observedRoutes, error) {
	return p.observeRoutes(ctx, gateway, func(tags []string, roles ...string) (Identity, error) {
		index, err := routeOwnerIndex(identities, tags, roles...)
		if err != nil {
			return Identity{}, err
		}
		return identities[index], nil
	})
}

func (p *Provider) observeOwnedGatewayRoutes(ctx context.Context, identity Identity, gateway cloud.GatewayState) (observedRoutes, error) {
	owners := make(map[string]cloud.Identity)
	return p.observeRoutes(ctx, gateway, func(tags []string, roles ...string) (Identity, error) {
		owner, err := identity.RouteIdentity(tags)
		if err != nil {
			return Identity{}, fmt.Errorf("%w: invalid Route identity during Gateway finalization", cloud.ErrOwnershipConflict)
		}
		for _, role := range roles {
			if !owner.MatchesRoute(tags, role) {
				continue
			}
			value := owner.Value()
			if existing, found := owners[value.RouteUID]; found && existing != value {
				return Identity{}, fmt.Errorf("%w: conflicting Route tuples during Gateway finalization", cloud.ErrOwnershipConflict)
			}
			owners[value.RouteUID] = value
			return owner, nil
		}
		return Identity{}, fmt.Errorf("%w: foreign Route resource during Gateway finalization", cloud.ErrOwnershipConflict)
	})
}

func (p *Provider) observeRoutes(ctx context.Context, gateway cloud.GatewayState, owner routeOwnerResolver) (observedRoutes, error) {
	graphs := make(observedRoutes)
	poolItems, err := p.octavia.ListPools(ctx, pools.ListOpts{LoadbalancerID: gateway.LoadBalancerID, ProjectID: p.projectID})
	if err != nil {
		return nil, fmt.Errorf("list pools beneath Gateway: %w", err)
	}
	sort.Slice(poolItems, func(i, j int) bool { return poolItems[i].ID < poolItems[j].ID })
	for index := range poolItems {
		pool := &poolItems[index]
		identity, err := owner(pool.Tags, rolePool)
		if err != nil {
			return nil, fmt.Errorf("validate pool %s: %w", pool.ID, err)
		}
		if err := p.validateGatewayDeletionPool(identity, *pool, gateway.LoadBalancerID); err != nil {
			return nil, &cloud.GraphError{Identity: identity.Value(), Err: err}
		}
		key, err := validatedChildKey(pool.Tags, cloud.ResourceRule)
		if err != nil {
			return nil, err
		}
		graph := graphs.rule(identity.Value().RouteUID, key.RuleIndex)
		if graph.pool != nil {
			return nil, &cloud.GraphError{Identity: identity.Value(), Err: fmt.Errorf("%w: duplicate pool rule key", cloud.ErrOwnershipConflict)}
		}
		graph.pool = pool
		if err := p.observeRoutePoolChildren(ctx, identity, graph); err != nil {
			return nil, &cloud.GraphError{Identity: identity.Value(), Err: err}
		}
	}
	for _, listener := range gateway.Listeners {
		items, err := p.octavia.ListPolicies(ctx, l7policies.ListOpts{ListenerID: listener.ID, ProjectID: p.projectID})
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
			identity, err := owner(item.Tags, rolePolicyExact, rolePolicyPrefix)
			if err != nil {
				return nil, fmt.Errorf("validate L7 policy %s: %w", item.ID, err)
			}
			observed, err := p.observeRoutePolicy(ctx, identity, item, listener.ID)
			if err != nil {
				return nil, &cloud.GraphError{Identity: identity.Value(), Err: err}
			}
			if observed.key.Listener != listener.Key {
				return nil, fmt.Errorf("%w: policy child key differs from its physical listener", cloud.ErrOwnershipConflict)
			}
			graph := graphs.rule(identity.Value().RouteUID, observed.key.RuleIndex)
			for _, policy := range graph.policies {
				if policy.key == observed.key {
					return nil, fmt.Errorf("%w: duplicate policy child key", cloud.ErrOwnershipConflict)
				}
			}
			if item.RedirectPoolID != "" && (graph.pool == nil || item.RedirectPoolID != graph.pool.ID) {
				return nil, fmt.Errorf("%w: policy references a pool outside its rule", cloud.ErrOwnershipConflict)
			}
			graph.policies = append(graph.policies, observed)
		}
	}
	return graphs, nil
}

func validatedChildKey(tags []string, kind cloud.ResourceKind) (cloud.ResourceKey, error) {
	key, err := openstackidentity.ChildKey(tags)
	if err != nil || key.Kind != kind {
		return cloud.ResourceKey{}, fmt.Errorf("%w: invalid or missing child resource key", cloud.ErrOwnershipConflict)
	}
	return key, nil
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
		key, err := validatedChildKey(member.Tags, cloud.ResourceRule)
		if err != nil || key.RuleIndex != graph.index {
			return fmt.Errorf("%w: member child key differs from its pool", cloud.ErrOwnershipConflict)
		}
		if member.PoolID != "" && member.PoolID != graph.pool.ID {
			return fmt.Errorf("%w: member %s reports pool %s instead of %s", cloud.ErrOwnershipConflict, member.ID, member.PoolID, graph.pool.ID)
		}
		address, err := netip.ParseAddr(member.Address)
		if err != nil || member.ProtocolPort < 1 || member.ProtocolPort > 65535 {
			return fmt.Errorf("%w: member %s has invalid address or port %s:%d", cloud.ErrOwnershipConflict, member.ID, member.Address, member.ProtocolPort)
		}
		member.Address = address.String()
		memberID := memberKey(member.Address, member.ProtocolPort)
		if existingID := memberKeys[memberID]; existingID != "" {
			return fmt.Errorf("%w: pool %s has duplicate members %s and %s for %s", cloud.ErrOwnershipConflict, graph.pool.ID, existingID, member.ID, key)
		}
		memberKeys[memberID] = member.ID
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
		key, err := validatedChildKey(monitor.Tags, cloud.ResourceRule)
		if err != nil || key.RuleIndex != graph.index {
			return fmt.Errorf("%w: monitor child key differs from its pool", cloud.ErrOwnershipConflict)
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
	role, err := routeResourceRole(identity, item.Tags, "policy", rolePolicyExact, rolePolicyPrefix)
	if err != nil {
		return observedRoutePolicy{}, fmt.Errorf("validate L7 policy %s: %w", item.ID, err)
	}
	key, err := validatedChildKey(item.Tags, cloud.ResourcePolicy)
	if err != nil || openstackidentity.ValidateChildRole(role, key) != nil {
		return observedRoutePolicy{}, fmt.Errorf("%w: policy role differs from its child key", cloud.ErrOwnershipConflict)
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
	ruleKeys := make(map[cloud.ResourceKey]string, len(ruleItems))
	for _, rule := range ruleItems {
		if err := p.validateOptionalProject("L7 rule", rule.ID, rule.ProjectID); err != nil {
			return observedRoutePolicy{}, err
		}
		ruleRole, err := routeResourceRole(identity, rule.Tags, "rule", roleRulePath, roleRuleHost, roleRuleHeader)
		if err != nil {
			return observedRoutePolicy{}, fmt.Errorf("validate L7 rule %s beneath policy %s: %w", rule.ID, item.ID, err)
		}
		ruleKey, err := validatedChildKey(rule.Tags, cloud.ResourcePredicate)
		if err != nil {
			return observedRoutePolicy{}, err
		}
		parentKey := ruleKey
		parentKey.Kind = cloud.ResourcePolicy
		parentKey.Predicate = ""
		if parentKey != key || openstackidentity.ValidateChildRole(ruleRole, ruleKey) != nil {
			return observedRoutePolicy{}, fmt.Errorf("%w: predicate child key differs from its policy or role", cloud.ErrOwnershipConflict)
		}
		if existingID := ruleKeys[ruleKey]; existingID != "" {
			return observedRoutePolicy{}, fmt.Errorf("%w: policy %s has duplicate %s rules %s and %s", cloud.ErrOwnershipConflict, item.ID, ruleRole, existingID, rule.ID)
		}
		ruleKeys[ruleKey] = rule.ID
		observedRules = append(observedRules, observedRouteRule{rule: rule, role: ruleRole, key: ruleKey})
	}
	return observedRoutePolicy{policy: item, role: role, key: key, rules: observedRules}, nil
}

func routeResourceRole(identity Identity, tags []string, resource string, roles ...string) (string, error) {
	for _, role := range roles {
		if identity.MatchesRoute(tags, role) {
			return role, nil
		}
		if identity.MatchesRouteDiscovery(tags, role) {
			return "", fmt.Errorf("%w: %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, resource)
		}
	}
	return "", fmt.Errorf("%w: %s is not owned by the selected HTTPRoute", cloud.ErrOwnershipConflict, resource)
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

func (p *Provider) executeRouteMutation(ctx context.Context, desired desiredRoute, step routeMutation) error {
	var err error
	switch step.kind {
	case routeMutationCreatePool:
		tags, tagErr := desired.identity.ChildTags(rolePool, cloud.ResourceKey{Kind: cloud.ResourceRule, RuleIndex: desired.rule.Index})
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
		tags, tagErr := desired.identity.ChildTags(roleMember, cloud.ResourceKey{Kind: cloud.ResourceRule, RuleIndex: desired.rule.Index})
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
			Weight:       desiredMemberWeight(desired.spec.MemberMode, step.member),
		})
	case routeMutationUpdateMember:
		_, err = p.octavia.UpdateMember(ctx, step.parentID, step.id, pools.UpdateMemberOpts{
			AdminStateUp: boolPointer(true),
			Weight:       desiredMemberWeight(desired.spec.MemberMode, step.member),
		})
	case routeMutationCreateMonitor:
		tags, tagErr := desired.identity.ChildTags(roleMonitor, cloud.ResourceKey{Kind: cloud.ResourceRule, RuleIndex: desired.rule.Index})
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
		tags, tagErr := desired.identity.ChildTags(step.policy.role, step.policy.key)
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreatePolicy(ctx, l7policies.CreateOpts{
			Name:           resourceName(desired.spec.Identity, step.policy.role),
			ListenerID:     desired.spec.Gateway.ListenerID(step.policy.key.Listener),
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
		tags, tagErr := desired.identity.ChildTags(step.rule.role, step.rule.key)
		if tagErr != nil {
			return tagErr
		}
		_, err = p.octavia.CreateRule(ctx, step.parentID, l7policies.CreateRuleOpts{
			RuleType:     step.rule.ruleType,
			CompareType:  step.rule.compareType,
			Key:          step.rule.header,
			Value:        step.rule.value,
			AdminStateUp: boolPointer(true),
			Tags:         tags,
		})
	case routeMutationUpdateRule:
		_, err = p.octavia.UpdateRule(ctx, step.parentID, step.id, l7policies.UpdateRuleOpts{
			RuleType:     step.rule.ruleType,
			CompareType:  step.rule.compareType,
			Key:          &step.rule.header,
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

// Endpoint weight represents readiness, not the Service backend's traffic share.
// NodePort mode retains Octavia's default and does not own member weights.
func desiredMemberWeight(mode cloud.MemberMode, member cloud.Member) *int {
	if mode != cloud.MemberModeEndpoint {
		return nil
	}
	weight := 1
	if member.Draining {
		weight = 0
	}
	return &weight
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
