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
	"strings"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

func TestEndpointMemberLifecyclePreservesRouteGraph(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	spec := completeGraphSpec(identity)
	spec.Routes[0].Spec.MemberMode = cloud.MemberModeEndpoint
	first := cloud.Member{Address: "10.244.0.10", Port: 8080}
	second := cloud.Member{Address: "10.244.1.20", Port: 8081}
	tags := safetyRouteTags(t, identity, roleMember)
	members := []pools.Member{
		{ID: "member-a", Address: first.Address, ProtocolPort: first.Port, SubnetID: "member-subnet", Weight: 1, AdminStateUp: true, Tags: tags},
		{ID: "member-b", Address: second.Address, ProtocolPort: second.Port, SubnetID: "member-subnet", Weight: 1, AdminStateUp: true, Tags: tags},
	}
	created := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		const path = "/lbaas/pools/pool-1/members"
		if !strings.HasPrefix(request.URL.Path, path) {
			base.ServeHTTP(w, request)
			return
		}
		requests.record(request)
		if request.Method == http.MethodGet {
			if request.URL.Query().Get("project_id") != "project-a" {
				t.Errorf("member discovery is not project scoped: %s", request.URL)
			}
			safetyWriteJSON(t, w, map[string]any{"members": members})
			return
		}
		id := strings.TrimPrefix(request.URL.Path, path+"/")
		index := slices.IndexFunc(members, func(member pools.Member) bool { return member.ID == id })
		switch request.Method {
		case http.MethodDelete:
			if index < 0 {
				t.Fatalf("deleted unknown member %s", id)
			}
			members = slices.Delete(members, index, index+1)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			var body struct {
				Member pools.Member `json:"member"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(body.Member.Tags, tags) || body.Member.SubnetID != "member-subnet" || body.Member.Weight != 1 {
				t.Fatalf("new endpoint lost identity, subnet, or traffic state: %+v", body.Member)
			}
			created++
			body.Member.ID = fmt.Sprintf("member-new-%d", created)
			members = append(members, body.Member)
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"member": body.Member})
		case http.MethodPut:
			if index < 0 {
				t.Fatalf("updated unknown member %s", id)
			}
			var body struct {
				Member struct {
					Weight       *int  `json:"weight"`
					AdminStateUp *bool `json:"admin_state_up"`
				} `json:"member"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Member.Weight == nil || body.Member.AdminStateUp == nil || !*body.Member.AdminStateUp {
				t.Fatal("member update omitted desired weight or administrative state")
			}
			members[index].Weight = *body.Member.Weight
			members[index].AdminStateUp = *body.Member.AdminStateUp
			safetyWriteJSON(t, w, map[string]any{"member": members[index]})
		default:
			t.Fatalf("unexpected member request: %s", request.Method)
		}
	})
	draining := first
	draining.Draining = true
	for _, step := range []struct {
		name      string
		members   []cloud.Member
		mutations int
		ids       []string
		weight    int
	}{
		{name: "two", members: []cloud.Member{first, second}, ids: []string{"member-a", "member-b"}, weight: 1},
		{name: "one", members: []cloud.Member{first}, mutations: 1, ids: []string{"member-a"}, weight: 1},
		{name: "drain", members: []cloud.Member{draining}, mutations: 1, ids: []string{"member-a"}, weight: 0},
		{name: "resume", members: []cloud.Member{first}, mutations: 1, ids: []string{"member-a"}, weight: 1},
		{name: "zero", mutations: 1},
		{name: "repopulate", members: []cloud.Member{first}, mutations: 1, ids: []string{"member-new-1"}, weight: 1},
	} {
		t.Run(step.name, func(t *testing.T) {
			spec.Routes[0].Spec.Members = step.members
			before := len(requests.mutationPaths())
			// Reconstruct the adapter on every pass to prove that cloud observation,
			// rather than an in-memory plan, drives the next transition.
			for attempt := 0; attempt <= step.mutations; attempt++ {
				result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec)
				if err != nil {
					t.Fatal(err)
				}
				state := result.Routes[0].Result.State
				if state.PoolID != "pool-1" || state.MonitorID != "monitor-1" || !reflect.DeepEqual(state.L7PolicyIDs, []string{"policy-1"}) {
					t.Fatalf("member transition replaced stable route resources: %+v", state)
				}
				if attempt == step.mutations && result.Outcome.State != cloud.OutcomeReady {
					t.Fatalf("final observation did not converge: %+v", result)
				}
			}
			if got := len(requests.mutationPaths()) - before; got != step.mutations {
				t.Fatalf("mutations = %d, want %d", got, step.mutations)
			}
			var ids []string
			for _, member := range members {
				ids = append(ids, member.ID)
				if member.Weight != step.weight {
					t.Errorf("member weight = %d, want %d", member.Weight, step.weight)
				}
			}
			if !slices.Equal(ids, step.ids) {
				t.Fatalf("member IDs = %v, want %v", ids, step.ids)
			}
		})
	}
}

func TestEndpointMemberValidationPrecedesCloudAccess(t *testing.T) {
	identity := safetyTestIdentity(t)
	for _, test := range []struct {
		name   string
		change func(*cloud.RouteSpec)
	}{
		{name: "unknown mode", change: func(spec *cloud.RouteSpec) { spec.MemberMode = "unknown" }},
		{name: "IPv6", change: func(spec *cloud.RouteSpec) { spec.Members[0].Address = "2001:db8::1" }},
		{name: "mapped IPv4", change: func(spec *cloud.RouteSpec) { spec.Members[0].Address = "::ffff:192.0.2.1" }},
		{name: "loopback", change: func(spec *cloud.RouteSpec) { spec.Members[0].Address = "127.0.0.1" }},
		{name: "link local", change: func(spec *cloud.RouteSpec) { spec.Members[0].Address = "169.254.169.254" }},
		{name: "multicast", change: func(spec *cloud.RouteSpec) { spec.Members[0].Address = "224.0.0.1" }},
		{name: "backend security", change: func(spec *cloud.RouteSpec) { spec.BackendSecurityGroupIDs = []string{"group"} }},
		{name: "worker identity", change: func(spec *cloud.RouteSpec) { spec.Members[0].ProviderID = "openstack:///server" }},
		{name: "conflicting duplicate", change: func(spec *cloud.RouteSpec) {
			duplicate := spec.Members[0]
			duplicate.Draining = true
			spec.Members = append(spec.Members, duplicate)
		}},
		{name: "NodePort empty", change: func(spec *cloud.RouteSpec) { spec.MemberMode, spec.Members = cloud.MemberModeNodePort, nil }},
		{name: "NodePort draining", change: func(spec *cloud.RouteSpec) {
			spec.MemberMode, spec.Members[0].Draining = cloud.MemberModeNodePort, true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := completeGraphSpec(identity)
			spec.Routes[0].Spec.MemberMode = cloud.MemberModeEndpoint
			test.change(&spec.Routes[0].Spec)
			reject := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				t.Errorf("cloud request for invalid endpoint input: %s %s", request.Method, request.URL)
			})
			provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(reject), Network: safetyServiceClient(reject), ProjectID: "project-a"}, ProviderConfig{})
			if _, err := provider.ReconcileGatewayGraph(context.Background(), spec); !errors.Is(err, cloud.ErrTerminalValidation) {
				t.Fatalf("error = %v, want terminal validation", err)
			}
		})
	}
}

func TestNodePortMemberWeightIsNotReconciled(t *testing.T) {
	desired, actual := convergedRoutePlanFixture()
	actual.members[0].Weight = 7
	plan, _, err := buildRouteEnsurePlan(desired, actual)
	if err != nil || len(plan) != 0 {
		t.Fatalf("NodePort weight caused a mutation: plan=%+v, error=%v", plan, err)
	}
}
