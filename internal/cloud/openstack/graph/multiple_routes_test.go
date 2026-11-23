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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/octavia"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

func TestGatewayGraphAssignsGlobalPolicyPositions(t *testing.T) {
	identity := safetyTestIdentity(t)
	spec := multipleRouteSpec(identity)
	spec.Routes[0].Spec.Rules[0].Matches[0].PathType, spec.Routes[0].Spec.Rules[0].Matches[0].PathValue = cloud.PathMatchPrefix, "/"
	spec.Routes[1].Spec.Rules[0].Matches[0].PathType, spec.Routes[1].Spec.Rules[0].Matches[0].PathValue = cloud.PathMatchPrefix, "/api"
	provider := completeGraphProvider(t, http.NotFoundHandler(), &gatewayTransitionRequests{})
	desired, err := provider.buildDesiredGatewayGraph(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := desired.routes[0][0].policies; len(got) != 1 || got[0].position != 3 {
		t.Fatalf("lower-priority default route = %#v", got)
	}
	if got := desired.routes[1][0].policies; len(got) != 2 || got[0].position != 1 || got[1].position != 2 || got[1].path != "/api/" {
		t.Fatalf("higher-priority path-element prefix route = %#v", got)
	}
	if spec.Routes[0].Spec.Rules[0].Matches[0].Priority != 7 || spec.Routes[1].Spec.Rules[0].Matches[0].Priority != 2 {
		t.Fatal("provider changed logical priorities in the input")
	}
	slices.Reverse(spec.Routes)
	reversed, err := provider.buildDesiredGatewayGraph(spec)
	if err != nil || !reflect.DeepEqual(desired, reversed) {
		t.Fatalf("route input order changed the desired graph: %v", err)
	}
}

func TestGatewayGraphRejectsInvalidPriorityBeforeCloudReads(t *testing.T) {
	for _, priority := range []int{-1, 7} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			spec := multipleRouteSpec(safetyTestIdentity(t))
			spec.Routes[1].Spec.Rules[0].Matches[0].Priority = priority
			reject := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				t.Errorf("cloud read before priority validation: %s", request.URL)
			})
			provider := completeGraphProvider(t, reject, &gatewayTransitionRequests{})
			if _, err := provider.ReconcileGatewayGraph(context.Background(), spec); !errors.Is(err, cloud.ErrTerminalValidation) {
				t.Fatalf("error=%v, want terminal validation", err)
			}
		})
	}
}

func TestGatewayGraphCreatesHigherPriorityRoutesFirst(t *testing.T) {
	provider, desired, actual := multipleRoutePlanFixture(t)
	actual.routes = nil
	plan, _, err := provider.buildGatewayGraphPlan(desired, actual)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 || plan[0].routeIndex != 1 || plan[0].route.kind != routeMutationCreatePool || plan[len(plan)-1].routeIndex != 0 {
		t.Fatalf("creation plan does not follow precedence: %#v", plan)
	}
}

func TestGatewayGraphWaitsForPreservedPolicyOrder(t *testing.T) {
	provider, desired, actual := multipleRoutePlanFixture(t)
	desired.spec.Routes[0].Disposition = cloud.GraphPreserve
	actual.routes["z-uid"][0].policies[0].policy.Position = 2
	plan, result, err := provider.buildGatewayGraphPlan(desired, actual)
	if err != nil || len(plan) != 0 || result.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("preserved policy authorized a shared listener update: plan=%#v, result=%#v, error=%v", plan, result, err)
	}
	// Detachment remains possible without changing the preserved route.
	desired.spec.Routes[1].Disposition = cloud.GraphDelete
	plan, _, err = provider.buildGatewayGraphPlan(desired, actual)
	if err != nil || len(plan) == 0 {
		t.Fatalf("detachment was blocked: plan=%#v, error=%v", plan, err)
	}
	for _, step := range plan {
		if step.kind != graphRouteMutation || step.routeIndex != 1 {
			t.Fatalf("detachment changed the preserved route: %#v", step)
		}
	}
}

func TestGatewayGraphDeletesBeforeReorderingSurvivors(t *testing.T) {
	provider, desired, actual := multipleRoutePlanFixture(t)
	desired.spec.Routes[1].Disposition = cloud.GraphDelete
	desired.routes[0][0].policies[0].position = 1
	plan, _, err := provider.buildGatewayGraphPlan(desired, actual)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 || plan[0].routeIndex != 1 || plan[0].route.kind != routeMutationDisablePolicy {
		t.Fatalf("survivor changed before detachment: %#v", plan)
	}
	// Octavia renumbers the survivor when the earlier policy is removed.
	// The following fresh observation requires no survivor update.
	actual.routes["z-uid"][0] = &observedRouteGraph{}
	actual.routes["a-uid"][0].policies[0].policy.Position = 1
	plan, _, err = provider.buildGatewayGraphPlan(desired, actual)
	if err != nil || len(plan) != 0 {
		t.Fatalf("deleted route caused unnecessary survivor mutation: plan=%#v, error=%v", plan, err)
	}
}

func TestReconcileMultipleRoutesConvergesWithoutFurtherMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	_, desired, actual := multipleRoutePlanFixture(t)
	// Reverse the observed priorities to exercise global reorder and Octavia's
	// automatic position adjustment of the other route's policy.
	actual.routes["a-uid"][0].policies[0].policy.Position = 1
	actual.routes["z-uid"][0].policies[0].policy.Position = 2
	requests := &gatewayTransitionRequests{}
	handler := multipleRouteHandler(t, requests, identity, &actual)
	for attempt := range 5 {
		before := len(requests.mutationPaths())
		// Recreate the provider to prove ordering never depends on process state.
		result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), desired.spec)
		if err != nil {
			t.Fatal(err)
		}
		mutations := len(requests.mutationPaths()) - before
		if attempt < 3 {
			if mutations != 1 || result.Outcome.State != cloud.OutcomeProgressing {
				t.Fatalf("reorder attempt %d: mutations=%d, result=%#v", attempt, mutations, result)
			}
		} else if mutations != 0 || result.Outcome.State != cloud.OutcomeReady {
			t.Fatalf("converged attempt %d: mutations=%d, result=%#v", attempt, mutations, result)
		}
	}
	if actual.routes["a-uid"][0].policies[0].policy.Position != 2 || actual.routes["z-uid"][0].policies[0].policy.Position != 1 {
		t.Fatalf("unexpected final policy positions: %#v", actual.routes)
	}
	for _, path := range requests.mutationPaths() {
		if path != "/lbaas/l7policies/policy-z" {
			t.Fatalf("global reorder changed an unrelated resource: %s", path)
		}
	}
}

func TestReconcileMultipleRoutesValidatesEveryRouteBeforeMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	_, desired, actual := multipleRoutePlanFixture(t)
	actual.routes["a-uid"][0].members[0].AdminStateUp = false
	actual.routes["z-uid"][0].members[0].Tags = []string{"foreign"}
	requests := &gatewayTransitionRequests{}
	handler := multipleRouteHandler(t, requests, identity, &actual)
	_, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), desired.spec)
	if !errors.Is(err, cloud.ErrOwnershipConflict) || len(requests.mutationPaths()) != 0 {
		t.Fatalf("mutated before validating the second route: error=%v, mutations=%v", err, requests.mutationPaths())
	}
}

func multipleRouteSpec(identity Identity) cloud.GatewayGraphSpec {
	spec := completeGraphSpec(identity)
	first := spec.Routes[0]
	first.Spec.Identity.RouteName, first.Spec.Identity.RouteUID = "a", "a-uid"
	first.Spec.Rules[0].Matches[0].Priority, first.Spec.Rules[0].Matches[0].Hostname = 7, ""
	second := first
	second.Spec.Rules = slices.Clone(first.Spec.Rules)
	second.Spec.Rules[0].Members = slices.Clone(first.Spec.Rules[0].Members)
	second.Spec.Rules[0].Matches = slices.Clone(first.Spec.Rules[0].Matches)
	second.Spec.Identity.RouteName, second.Spec.Identity.RouteUID = "z", "z-uid"
	second.Spec.Rules[0].Matches[0].Priority = 2
	spec.Routes = []cloud.RouteGraphNode{first, second}
	return spec
}

func multipleRoutePlanFixture(t *testing.T) (*Provider, desiredGatewayGraph, observedGatewayGraph) {
	t.Helper()
	provider := completeGraphProvider(t, http.NotFoundHandler(), &gatewayTransitionRequests{})
	desired, err := provider.buildDesiredGatewayGraph(multipleRouteSpec(safetyTestIdentity(t)))
	if err != nil {
		t.Fatal(err)
	}
	actual := observedGatewayGraph{
		routes:       make(observedRoutes),
		state:        desired.routes[0][0].spec.Gateway,
		loadBalancer: &octavia.LoadBalancer{}, listeners: map[cloud.ListenerKey]*listeners.Listener{{Protocol: cloud.HTTPProtocol, Port: 80}: {ID: "listener-1", AdminStateUp: true}},
	}
	actual.loadBalancer.AdminStateUp = true
	for _, rules := range desired.routes {
		route := rules[0]
		_, observed := convergedRoutePlanFixture()
		name := route.spec.Identity.RouteName
		observed.pool.ID, observed.pool.Tags = "pool-"+name, safetyRouteTags(t, route.identity, rolePool)
		observed.members[0].ID, observed.members[0].Tags = "member-"+name, safetyRouteTags(t, route.identity, roleMember)
		observed.monitor.ID, observed.monitor.Tags = "monitor-"+name, safetyRouteTags(t, route.identity, roleMonitor)
		policy := &observed.policies[0]
		policy.policy.ID, policy.policy.ListenerID, policy.policy.RedirectPoolID = "policy-"+name, "listener-1", observed.pool.ID
		policy.policy.Position, policy.policy.Tags = route.policies[0].position, safetyRouteTags(t, route.identity, rolePolicyExact)
		policy.rules[0].rule.ID, policy.rules[0].rule.Tags = "rule-"+name, safetyRouteTags(t, route.identity, roleRulePath)
		actual.routes.rule(route.spec.Identity.RouteUID, 0)
		*actual.routes[route.spec.Identity.RouteUID][0] = observed
	}
	return provider, desired, actual
}

func multipleRouteHandler(t *testing.T, requests *gatewayTransitionRequests, identity Identity, actual *observedGatewayGraph) http.Handler {
	t.Helper()
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		if !strings.HasPrefix(path, "/lbaas/pools") && !strings.HasPrefix(path, "/lbaas/l7policies") && path != "/lbaas/healthmonitors" {
			base.ServeHTTP(w, request)
			return
		}
		requests.record(request)
		if request.Method == http.MethodGet && path == "/lbaas/pools" {
			var items []*pools.Pool
			for _, rules := range actual.routes {
				route := rules[0]
				items = append(items, route.pool)
			}
			safetyWriteJSON(t, w, map[string]any{"pools": items})
			return
		}
		var policies []*l7policies.L7Policy
		for i := range actual.routes {
			route := actual.routes[i][0]
			if request.Method == http.MethodGet && path == "/lbaas/pools/"+route.pool.ID+"/members" {
				safetyWriteJSON(t, w, map[string]any{"members": route.members})
				return
			}
			if request.Method == http.MethodGet && path == "/lbaas/healthmonitors" && request.URL.Query().Get("pool_id") == route.pool.ID {
				safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{route.monitor}})
				return
			}
			for j := range route.policies {
				policy := &route.policies[j]
				policies = append(policies, &policy.policy)
				if request.Method == http.MethodGet && path == "/lbaas/l7policies/"+policy.policy.ID+"/rules" {
					var rules []l7policies.Rule
					for _, rule := range policy.rules {
						rules = append(rules, rule.rule)
					}
					safetyWriteJSON(t, w, map[string]any{"rules": rules})
					return
				}
			}
		}
		if request.Method == http.MethodGet && path == "/lbaas/l7policies" {
			safetyWriteJSON(t, w, map[string]any{"l7policies": policies})
			return
		}
		if request.Method == http.MethodPut {
			var body struct {
				Policy l7policies.UpdateOpts `json:"l7policy"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			sort.Slice(policies, func(i, j int) bool { return policies[i].Position < policies[j].Position })
			for index, policy := range policies {
				if path != "/lbaas/l7policies/"+policy.ID {
					continue
				}
				if body.Policy.AdminStateUp != nil {
					policy.AdminStateUp = *body.Policy.AdminStateUp
				}
				if body.Policy.Position > 0 {
					policies = slices.Delete(policies, index, index+1)
					position := min(int(body.Policy.Position)-1, len(policies))
					policies = slices.Insert(policies, position, policy)
					for i, item := range policies {
						item.Position = int32(i + 1)
					}
				}
				safetyWriteJSON(t, w, map[string]any{"l7policy": policy})
				return
			}
		}
		t.Errorf("unexpected request: %s %s", request.Method, request.URL)
		http.Error(w, "unexpected request", http.StatusNotFound)
	})
}
