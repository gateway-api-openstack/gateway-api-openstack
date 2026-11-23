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
	"net/http"
	"reflect"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

func TestBuildRouteEnsurePlanDisablesPolicyBeforeChangingRules(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	desired.policies[0].match.Hostname = "api.example.test"

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	if len(plan) == 0 || plan[0] != (routeMutation{kind: routeMutationDisablePolicy, id: "policy-1"}) {
		t.Fatalf("first route mutation = %#v, want policy disable", firstRouteMutation(plan))
	}
}

func TestBuildRouteEnsurePlanPreservesUnchangedListenerPolicy(t *testing.T) {
	tests := []struct {
		name   string
		change func(*desiredRoute, *observedRouteGraph)
		first  routeMutationKind
	}{
		{
			name: "add match",
			change: func(_ *desiredRoute, actual *observedRouteGraph) {
				actual.policies = actual.policies[1:]
			},
			first: routeMutationCreatePolicy,
		},
		{
			name: "change match",
			change: func(desired *desiredRoute, _ *observedRouteGraph) {
				desired.policies[0].path = "/changed"
			},
			first: routeMutationDisablePolicy,
		},
		{
			name: "remove match",
			change: func(desired *desiredRoute, _ *observedRouteGraph) {
				desired.policies = desired.policies[1:]
			},
			first: routeMutationDisablePolicy,
		},
		{
			name: "add predicate",
			change: func(desired *desiredRoute, _ *observedRouteGraph) {
				desired.policies[0].match.Hostname = "api.example.test"
			},
			first: routeMutationDisablePolicy,
		},
		{
			name: "repair redirect target",
			change: func(_ *desiredRoute, actual *observedRouteGraph) {
				actual.policies[0].policy.RedirectPoolID = ""
			},
			first: routeMutationDisablePolicy,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired, actual := convergedRoutePlanFixture()
			sibling, observedSibling := convergedRoutePlanFixture()
			sibling.policies[0].key.Listener.Port = 8080
			sibling.policies[0].match.Listener.Port = 8080
			observedSibling.policies[0].key.Listener.Port = 8080
			observedSibling.policies[0].policy.ID = "unchanged-policy"
			observedSibling.policies[0].rules[0].key.Listener.Port = 8080
			observedSibling.policies[0].rules[0].rule.ID = "unchanged-rule"
			desired.policies = append(desired.policies, sibling.policies[0])
			actual.policies = append(actual.policies, observedSibling.policies[0])
			test.change(&desired, &actual)

			plan, _, err := buildRouteEnsurePlan(desired, actual)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan) == 0 || plan[0].kind != test.first {
				t.Fatalf("first mutation = %#v, want kind %v", firstRouteMutation(plan), test.first)
			}
			for _, step := range plan {
				if step.id == "unchanged-policy" || step.parentID == "unchanged-policy" || step.id == "unchanged-rule" {
					t.Fatalf("unchanged listener policy appears in mutation plan: %#v", step)
				}
			}
		})
	}
}

func TestGatewayGraphRejectsPolicyWithoutVerifiedRedirectPool(t *testing.T) {
	for _, disposition := range []cloud.GraphDisposition{cloud.GraphEnsure, cloud.GraphDelete} {
		t.Run(string(disposition), func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			base := convergedRouteGraphHandler(t, requests, identity, true, true)
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools" {
					requests.record(request)
					safetyWriteJSON(t, w, map[string]any{"pools": []any{}})
					return
				}
				base.ServeHTTP(w, request)
			})
			spec := completeGraphSpec(identity)
			spec.Gateway.Disposition = disposition
			if disposition == cloud.GraphDelete {
				spec.Routes = nil
			}
			_, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec)
			if !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("unverified redirect target error = %v, want ownership conflict", err)
			}
			if mutations := requests.mutationPaths(); len(mutations) != 0 {
				t.Fatalf("mutated graph before verifying redirect target: %v", mutations)
			}
		})
	}
}

func TestBuildRouteEnsurePlanEnablesOnlyConvergedPolicy(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.policies[0].policy.AdminStateUp = false

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := []routeMutation{{kind: routeMutationEnablePolicy, id: "policy-1"}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("route plan = %#v, want %#v", plan, want)
	}
}

func TestBuildRouteEnsurePlanReturnsDeterministicConvergedState(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()

	plan, state, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	if len(plan) != 0 {
		t.Fatalf("converged route plan = %#v, want no mutations", plan)
	}
	want := cloud.RouteRuleState{PoolID: "pool-1", MemberIDs: []string{"member-1"}, MonitorID: "monitor-1", L7PolicyIDs: []string{"policy-1"}, L7RuleIDs: []string{"rule-path"}}

	if !reflect.DeepEqual(state, want) {
		t.Fatalf("converged route state = %#v, want %#v", state, want)
	}
}

func TestBuildRouteEnsurePlanRepairsDisabledMember(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.members[0].AdminStateUp = false

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := []routeMutation{{
		kind:     routeMutationUpdateMember,
		id:       "member-1",
		parentID: "pool-1",
		member:   desired.members[0],
	}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("route plan = %#v, want %#v", plan, want)
	}
}

func TestBuildRouteEnsurePlanRecreatesDeletedMember(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.members = nil

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := routeMutation{
		kind:     routeMutationCreateMember,
		parentID: "pool-1",
		member:   cloud.Member{Address: "10.0.0.10", Port: 30080},
	}
	if !reflect.DeepEqual(plan, []routeMutation{want}) {
		t.Fatalf("route mutations = %#v, want only %#v", plan, want)
	}
}

func TestBuildRouteEnsurePlanRecreatesDeletedMonitor(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.monitor = nil

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := routeMutation{kind: routeMutationCreateMonitor, parentID: "pool-1"}
	if !reflect.DeepEqual(plan, []routeMutation{want}) {
		t.Fatalf("route mutations = %#v, want only %#v", plan, want)
	}
}

func TestBuildRouteEnsurePlanRecreatesDeletedPolicy(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.policies = nil

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := routeMutation{
		kind:     routeMutationCreatePolicy,
		parentID: "pool-1",
		policy:   desired.policies[0],
	}
	if !reflect.DeepEqual(plan, []routeMutation{want}) {
		t.Fatalf("route mutations = %#v, want only %#v", plan, want)
	}
}

func TestBuildRouteEnsurePlanDisablesPolicyBeforeRecreatingDeletedRule(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.policies[0].rules = nil

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := []routeMutation{
		{kind: routeMutationDisablePolicy, id: "policy-1"},
		{
			kind:     routeMutationCreateRule,
			parentID: "policy-1",
			rule: desiredRule{
				role:        roleRulePath,
				key:         testChildKey(roleRulePath),
				ruleType:    l7policies.TypePath,
				compareType: l7policies.CompareTypeEqual,
				value:       "/api",
			},
		},
	}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("route mutations = %#v, want %#v", plan, want)
	}
}

func TestBuildRouteEnsurePlanDisablesPolicyBeforeRecreatingDeletedPool(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.pool = nil
	actual.members = nil
	actual.monitor = nil

	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatalf("buildRouteEnsurePlan() error = %v", err)
	}
	want := []routeMutation{
		{kind: routeMutationDisablePolicy, id: "policy-1"},
		{kind: routeMutationCreatePool},
	}
	if len(plan) < len(want) || !reflect.DeepEqual(plan[:len(want)], want) {
		t.Fatalf("first route mutations = %#v, want %#v", plan, want)
	}
}

func TestBuildRouteDeletionPlanDisablesPolicyBeforeDeletingRules(t *testing.T) {
	_, actual := convergedRoutePlanFixture()
	plan := buildRouteDeletionPlan(actual)
	if len(plan) < 2 {
		t.Fatalf("route deletion plan = %#v, want policy disable and rule deletion", plan)
	}
	if plan[0] != (routeMutation{kind: routeMutationDisablePolicy, id: "policy-1"}) {
		t.Fatalf("first route deletion = %#v, want policy disable", plan[0])
	}
	if plan[1] != (routeMutation{kind: routeMutationDeleteRule, id: "rule-path", parentID: "policy-1"}) {
		t.Fatalf("second route deletion = %#v, want path rule deletion", plan[1])
	}
}

func convergedRoutePlanFixture() (desiredRoute, observedRouteGraph) {
	policy := desiredPolicy{
		role:        rolePolicyExact,
		key:         testChildKey(rolePolicyExact),
		match:       &cloud.RouteMatchSpec{Listener: cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 80}},
		position:    1,
		compareType: l7policies.CompareTypeEqual,
		path:        "/api",
	}
	desired := desiredRoute{
		spec: cloud.RouteSpec{
			MemberSubnetID: "member-subnet", Rules: []cloud.RouteRuleSpec{{Matches: []cloud.RouteMatchSpec{{Listener: cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 80}, PathType: cloud.PathMatchExact, PathValue: "/api"}}}},
		},
		members:       []cloud.Member{{Address: "10.0.0.10", Port: 30080}},
		healthMonitor: cloud.HealthMonitorConfig{Path: "/healthz", Delay: 10, Timeout: 5, MaxRetries: 3, MaxRetriesDown: 3},
		policies:      []desiredPolicy{policy},
	}
	actual := observedRouteGraph{
		pool: &pools.Pool{
			ID:           "pool-1",
			Protocol:     string(pools.ProtocolHTTP),
			LBMethod:     string(pools.LBMethodRoundRobin),
			AdminStateUp: true,
		},
		members: []pools.Member{{
			ID: "member-1", Address: "10.0.0.10", ProtocolPort: 30080, SubnetID: "member-subnet", AdminStateUp: true,
		}},
		monitor: &monitors.Monitor{
			ID:             "monitor-1",
			Type:           monitors.TypeHTTP,
			Delay:          desired.healthMonitor.Delay,
			Timeout:        desired.healthMonitor.Timeout,
			MaxRetries:     desired.healthMonitor.MaxRetries,
			MaxRetriesDown: desired.healthMonitor.MaxRetriesDown,
			URLPath:        desired.healthMonitor.Path,
			HTTPMethod:     http.MethodGet,
			ExpectedCodes:  routeMonitorExpectedCodes,
			AdminStateUp:   true,
		},
		policies: []observedRoutePolicy{{
			role: rolePolicyExact,
			key:  testChildKey(rolePolicyExact),
			policy: l7policies.L7Policy{
				ID: "policy-1", Action: string(l7policies.ActionRedirectToPool), RedirectPoolID: "pool-1",
				Position: 1, AdminStateUp: true,
			},
			rules: []observedRouteRule{{
				role: roleRulePath,
				key:  testChildKey(roleRulePath),
				rule: l7policies.Rule{
					ID: "rule-path", RuleType: string(l7policies.TypePath),
					CompareType: string(l7policies.CompareTypeEqual), Value: "/api", AdminStateUp: true,
				},
			}},
		}},
	}
	return desired, actual
}

func firstRouteMutation(plan []routeMutation) routeMutation {
	if len(plan) == 0 {
		return routeMutation{}
	}
	return plan[0]
}

func TestMemberProviderIdentityDoesNotChangeOctaviaMembership(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	desired.members[0].ProviderID = "openstack:///11111111-1111-1111-1111-111111111111"
	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("provider identity changed a converged member: %#v", plan)
	}
}
