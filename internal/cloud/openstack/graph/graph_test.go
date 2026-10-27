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
	"strings"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/octavia"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
)

func completeGraphSpec(identity Identity) cloud.GatewayGraphSpec {
	id := identity.Value()
	id.RouteName, id.RouteNamespace, id.RouteUID = "", "", ""
	gateway := gatewayTransitionSpec(identity)
	gateway.Identity = id
	return cloud.GatewayGraphSpec{
		Identity: id,
		Gateway:  cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure, Spec: gateway},
		Routes:   []cloud.RouteGraphNode{{Disposition: cloud.GraphEnsure, Spec: routeTransitionSpec(identity)}},
	}
}

func completeGraphProvider(t *testing.T, handler http.Handler, requests *gatewayTransitionRequests) *Provider {
	t.Helper()
	return newTestProvider(t, ServiceClients{
		LoadBalancer: safetyServiceClient(handler), Network: safetyServiceClient(routeTransitionEmptyFloatingIPHandler(t, requests)), ProjectID: "project-a",
	}, ProviderConfig{})
}

func TestReconcileGatewayGraphValidatesAllDesiredComponentsBeforeCloudReads(t *testing.T) {
	identity := safetyTestIdentity(t)
	tests := []struct {
		name   string
		change func(*cloud.GatewayGraphSpec)
		want   error
	}{
		{name: "invalid backend", change: func(spec *cloud.GatewayGraphSpec) { spec.Routes[0].Spec.Members[0].Port = 0 }, want: cloud.ErrTerminalValidation},
		{name: "duplicate fragment", change: func(spec *cloud.GatewayGraphSpec) { spec.Routes = append(spec.Routes, spec.Routes[0]) }, want: cloud.ErrOwnershipConflict},
		{name: "different topology", change: func(spec *cloud.GatewayGraphSpec) { spec.Routes[0].Spec.GatewayRequirements.VIPSubnetID = "other" }, want: cloud.ErrOwnershipConflict},
		{name: "invalid monitor", change: func(spec *cloud.GatewayGraphSpec) {
			spec.Routes[0].Spec.HealthMonitor.Timeout = spec.Routes[0].Spec.HealthMonitor.Delay
		}, want: cloud.ErrTerminalValidation},
		{name: "missing monitor", change: func(spec *cloud.GatewayGraphSpec) {
			spec.Routes[0].Spec.HealthMonitor = cloud.HealthMonitorConfig{}
		}, want: cloud.ErrTerminalValidation},
		{name: "missing monitor path", change: func(spec *cloud.GatewayGraphSpec) { spec.Routes[0].Spec.HealthMonitor.Path = "" }, want: cloud.ErrTerminalValidation},
		{name: "missing monitor delay", change: func(spec *cloud.GatewayGraphSpec) { spec.Routes[0].Spec.HealthMonitor.Delay = 0 }, want: cloud.ErrTerminalValidation},
		{name: "missing monitor timeout", change: func(spec *cloud.GatewayGraphSpec) { spec.Routes[0].Spec.HealthMonitor.Timeout = 0 }, want: cloud.ErrTerminalValidation},
		{name: "missing monitor retries", change: func(spec *cloud.GatewayGraphSpec) { spec.Routes[0].Spec.HealthMonitor.MaxRetries = 0 }, want: cloud.ErrTerminalValidation},
		{name: "missing monitor retries down", change: func(spec *cloud.GatewayGraphSpec) { spec.Routes[0].Spec.HealthMonitor.MaxRetriesDown = 0 }, want: cloud.ErrTerminalValidation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := completeGraphSpec(identity)
			test.change(&spec)
			rejectRequest := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				t.Errorf("cloud request before complete desired validation: %s", request.URL)
			})
			provider := newTestProvider(t, ServiceClients{
				LoadBalancer: safetyServiceClient(rejectRequest), Network: safetyServiceClient(rejectRequest), ProjectID: "project-a",
			}, ProviderConfig{})
			if _, err := provider.ReconcileGatewayGraph(context.Background(), spec); !errors.Is(err, test.want) {
				t.Fatalf("validation error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestReconcileGatewayGraphConvergedPassesMakeNoMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	handler := convergedRouteGraphHandler(t, requests, identity, true, true)
	for range 2 {
		// A new adapter cannot depend on an in-memory plan or fragment registry.
		result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), completeGraphSpec(identity))
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome.State != cloud.OutcomeReady || result.Gateway.Outcome.State != cloud.OutcomeReady || len(result.Routes) != 1 || result.Routes[0].Result.Outcome.State != cloud.OutcomeReady {
			t.Fatalf("converged graph result = %#v", result)
		}
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("converged mutations = %v", got)
	}
	for _, path := range []string{"/lbaas/loadbalancers/load-balancer-1", "/lbaas/listeners", "/lbaas/pools", "/lbaas/l7policies", "/lbaas/pools/pool-1/members"} {
		if got := requests.count(http.MethodGet, path); got != 2 {
			t.Errorf("observations of %s = %d, want one per call", path, got)
		}
	}
}

func TestReconcileGatewayGraphRejectsUnaccountedChildrenBeforeGatewayRepair(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	spec := completeGraphSpec(identity)
	spec.Routes = nil
	provider := completeGraphProvider(t, convergedRouteGraphHandler(t, requests, identity, false, false), requests)
	_, err := provider.ReconcileGatewayGraph(context.Background(), spec)
	if !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("error = %v, want ownership conflict", err)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutations with unaccounted children = %v", got)
	}
}

func TestReconcileGatewayGraphAttributesRouteFailureWithoutRejectingProgrammedGateway(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/lbaas/pools/pool-1/members" {
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"members": []any{map[string]any{"id": "foreign-member", "tags": []string{"foreign"}}}})
			return
		}
		base.ServeHTTP(w, request)
	})
	result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), completeGraphSpec(identity))
	var component *cloud.GraphError
	if !errors.As(err, &component) || component.Identity.RouteUID != identity.Value().RouteUID || !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("error = %#v, want exact route ownership conflict", err)
	}
	if result.Gateway.Outcome.State != cloud.OutcomeReady {
		t.Fatalf("unaffected Gateway result = %#v", result.Gateway)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutations after route conflict = %v", got)
	}
}

func TestReconcileGatewayGraphPreserveCannotRestoreTraffic(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	spec := completeGraphSpec(identity)
	spec.Routes[0].Disposition = cloud.GraphPreserve
	result, err := completeGraphProvider(t, convergedRouteGraphHandler(t, requests, identity, false, false), requests).ReconcileGatewayGraph(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("result = %#v, want held graph", result)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("preserved fragment authorized parent repair: %v", got)
	}
}

func TestReconcileGatewayGraphMissingGatewayCleanupIsReadOnly(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	spec := completeGraphSpec(identity)
	spec.Gateway.Disposition, spec.Routes[0].Disposition = cloud.GraphPreserve, cloud.GraphDelete
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		if request.Method != http.MethodGet || request.URL.Path != "/lbaas/loadbalancers" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
		}
		safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{}})
	})
	result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != cloud.OutcomeReady || result.Routes[0].Result.Outcome.State != cloud.OutcomeReady {
		t.Fatalf("cleanup result = %#v", result)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("cleanup recreated absent Gateway: %v", got)
	}
}

func TestReconcileGatewayGraphPreservesFloatingIPQuotaClassification(t *testing.T) {
	for _, mutation := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(mutation, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			spec := completeGraphSpec(identity)
			if mutation == http.MethodPost {
				spec.Gateway.Spec.ExternalNetworkID = "external-network"
				spec.Routes[0].Spec.GatewayRequirements.ExternalNetworkID = "external-network"
			}
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				if request.Method == http.MethodGet && request.URL.Path == "/floatingips" {
					items := []any{}
					if mutation == http.MethodDelete {
						items = append(items, map[string]any{
							"id": "floating-ip-1", "project_id": "project-a", "port_id": "vip-port", "fixed_ip_address": "192.0.2.10",
							"floating_network_id": "external-network", "description": identity.GatewayDescription(roleFloatingIP),
						})
					}
					safetyWriteJSON(t, w, map[string]any{"floatingips": items})
					return
				}
				if request.Method != mutation {
					t.Fatalf("unexpected network request: %s %s", request.Method, request.URL)
				}
				w.WriteHeader(http.StatusConflict)
				safetyWriteJSON(t, w, map[string]any{"NeutronError": map[string]any{"type": "OverQuota", "message": "Quota exceeded for floatingip"}})
			})
			provider := newTestProvider(t, ServiceClients{
				LoadBalancer: safetyServiceClient(convergedRouteGraphHandler(t, requests, identity, true, true)),
				Network:      safetyServiceClient(network), ProjectID: "project-a",
			}, ProviderConfig{})
			if _, err := provider.ReconcileGatewayGraph(context.Background(), spec); !errors.Is(err, cloud.ErrQuotaExceeded) {
				t.Fatalf("error = %v, want quota exceeded", err)
			}
			if got := requests.mutationPaths(); len(got) != 1 {
				t.Fatalf("mutations = %v, want one failed Floating IP mutation", got)
			}
		})
	}
}

func TestReconcileGatewayGraphReplacesAllMembersAcrossFreshObservations(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	spec := completeGraphSpec(identity)
	spec.Routes[0].Spec.Members = []cloud.Member{{Address: "10.0.0.20", Port: 30081}, {Address: "10.0.0.30", Port: 30081}}
	members := []map[string]any{
		{"id": "member-old-a", "address": "10.0.0.10", "protocol_port": 30080, "subnet_id": "member-subnet", "admin_state_up": true, "tags": safetyRouteTags(t, identity, roleMember)},
		{"id": "member-old-b", "address": "10.0.0.11", "protocol_port": 30080, "subnet_id": "member-subnet", "admin_state_up": true, "tags": safetyRouteTags(t, identity, roleMember)},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		path := "/lbaas/pools/pool-1/members"
		if !strings.HasPrefix(request.URL.Path, path) {
			base.ServeHTTP(w, request)
			return
		}
		requests.record(request)
		switch request.Method {
		case http.MethodGet:
			safetyWriteJSON(t, w, map[string]any{"members": members})
		case http.MethodDelete:
			id := strings.TrimPrefix(request.URL.Path, path+"/")
			for index, member := range members {
				if member["id"] == id {
					members = append(members[:index], members[index+1:]...)
					break
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			var body struct {
				Member map[string]any `json:"member"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			body.Member["id"] = fmt.Sprintf("member-new-%d", len(members))
			members = append(members, body.Member)
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"member": body.Member})
		default:
			t.Fatalf("unexpected member mutation: %s", request.Method)
		}
	})
	for step, count := range []int{1, 0, 1, 2, 2} {
		before := len(requests.mutationPaths())
		result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec)
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if len(members) != count {
			t.Fatalf("step %d member count = %d, want %d", step, len(members), count)
		}
		wantMutations := 1
		if step == 4 {
			wantMutations = 0
		}
		if got := len(requests.mutationPaths()) - before; got != wantMutations {
			t.Fatalf("step %d mutations = %d, want %d", step, got, wantMutations)
		}
		if result.Gateway.Outcome.State != cloud.OutcomeReady || result.Routes[0].Result.State.PoolID != "pool-1" || !reflect.DeepEqual(result.Routes[0].Result.State.L7PolicyIDs, []string{"policy-1"}) {
			t.Fatalf("step %d changed stable Gateway, pool or policy: %#v", step, result)
		}
		if step == 4 && result.Outcome.State != cloud.OutcomeReady {
			t.Fatalf("last pass did not converge: %#v", result)
		}
	}
}

func TestGatewayGraphPlanDetachesBeforeEnsuringAndRestoresParentsLast(t *testing.T) {
	provider := &Provider{}
	route, actualRoute := convergedRoutePlanFixture()
	old := route
	old.spec.Identity.RouteUID = "old-route"
	desired := desiredGatewayGraph{spec: cloud.GatewayGraphSpec{
		Gateway: cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure},
		Routes:  []cloud.RouteGraphNode{{Disposition: cloud.GraphEnsure}, {Disposition: cloud.GraphDelete}},
	}, routes: []desiredRoute{route, old}}
	actual := observedGatewayGraph{
		loadBalancer: &octavia.LoadBalancer{}, listener: &listeners.Listener{},
		routes: []observedRouteGraph{{}, actualRoute},
	}
	plan, _, err := provider.buildGatewayGraphPlan(desired, actual)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) < 4 || plan[0].kind != graphRouteMutation || plan[0].routeIndex != 1 || plan[0].route.kind != routeMutationDisablePolicy {
		t.Fatalf("first mutation must detach old fragment: %#v", plan)
	}
	if plan[len(plan)-2].kind != graphEnableListener || plan[len(plan)-1].kind != graphEnableLoadBalancer {
		t.Fatalf("parent repair order = %#v", plan)
	}
}

func TestReconcileGatewayGraphPendingRouteKeepsGatewayProgrammed(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/lbaas/loadbalancers/load-balancer-1" {
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": gatewayTransitionLoadBalancer(safetyGatewayTags(t, identity, roleLoadBalancer), "PENDING_UPDATE")})
			return
		}
		base.ServeHTTP(w, request)
	})
	result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), completeGraphSpec(identity))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing || result.Gateway.Outcome.State != cloud.OutcomeReady || result.Routes[0].Result.Outcome.State != cloud.OutcomeProgressing {
		t.Fatalf("pending route changed Gateway component readiness: %#v", result)
	}
	if got := requests.mutationPaths(); len(got) != 0 {
		t.Fatalf("mutations while pending: %v", got)
	}
	if got := requests.count(http.MethodGet, "/lbaas/pools"); got != 0 {
		t.Fatalf("pending graph unnecessarily read route descendants: %d", got)
	}
}

func TestReconcileGatewayGraphObservesAttachedAndDetachingFragmentsTogether(t *testing.T) {
	identity := safetyTestIdentity(t)
	oldIdentityValue := identity.Value()
	oldIdentityValue.RouteName, oldIdentityValue.RouteUID = "old-route", "old-route-uid"
	oldIdentity, err := NewIdentity(oldIdentityValue)
	if err != nil {
		t.Fatal(err)
	}
	requests := &gatewayTransitionRequests{}
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"pools": []any{
				map[string]any{"id": "pool-1", "protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN", "admin_state_up": true, "tags": safetyRouteTags(t, identity, rolePool)},
				map[string]any{"id": "old-pool", "protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN", "admin_state_up": true, "tags": safetyRouteTags(t, oldIdentity, rolePool)},
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/old-pool/members":
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"members": []any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors" && request.URL.Query().Get("pool_id") == "old-pool":
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{}})
		case request.Method == http.MethodDelete && request.URL.Path == "/lbaas/pools/old-pool":
			requests.record(request)
			if requests.count(http.MethodGet, "/lbaas/l7policies/policy-1/rules") != 1 {
				t.Error("deleted old fragment before observing attached fragment")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			base.ServeHTTP(w, request)
		}
	})
	spec := completeGraphSpec(identity)
	spec.Routes = append(spec.Routes, cloud.RouteGraphNode{Disposition: cloud.GraphDelete, Spec: cloud.RouteSpec{Identity: oldIdentityValue}})
	result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := requests.mutationPaths(); !reflect.DeepEqual(got, []string{"/lbaas/pools/old-pool"}) {
		t.Fatalf("mutations = %v", got)
	}
	if result.Gateway.Outcome.State != cloud.OutcomeReady || len(result.Routes) != 2 {
		t.Fatalf("component results = %#v", result)
	}
}

func TestReconcileGatewayGraphDeletionValidatesEveryChildBeforeOneMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			requests.record(request)
			if request.URL.Path != "/lbaas/l7policies/policy-1/rules/rule-host" {
				t.Errorf("first deletion = %s", request.URL.Path)
			}
			if requests.count(http.MethodGet, "/lbaas/pools/pool-1/members") != 1 {
				t.Error("deleted rule before validating pool descendants")
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		base.ServeHTTP(w, request)
	})
	spec := completeGraphSpec(identity)
	spec.Gateway.Disposition = cloud.GraphDelete
	spec.Routes = nil
	result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing || len(requests.mutationPaths()) != 1 {
		t.Fatalf("delete result = %#v, mutations = %v", result, requests.mutationPaths())
	}
}

func TestReconcileGatewayGraphRecoversLostPoolCreateResponse(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	poolExists := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools":
			requests.record(request)
			items := []any{}
			if poolExists {
				items = append(items, map[string]any{"id": "pool-1", "protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN", "admin_state_up": true, "tags": safetyRouteTags(t, identity, rolePool)})
			}
			safetyWriteJSON(t, w, map[string]any{"pools": items})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/pools":
			requests.record(request)
			poolExists = true
			http.Error(w, "create response lost", http.StatusInternalServerError)
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/pools/pool-1/members":
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"members": []any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/healthmonitors":
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"healthmonitors": []any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"l7policies": []any{}})
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/pools/pool-1/members":
			requests.record(request)
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"member": map[string]any{"id": "member-1"}})
		default:
			base.ServeHTTP(w, request)
		}
	})
	spec := completeGraphSpec(identity)
	if _, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec); err == nil {
		t.Fatal("expected lost response error")
	}
	result, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.State != cloud.OutcomeProgressing || requests.count(http.MethodPost, "/lbaas/pools") != 1 || requests.count(http.MethodPost, "/lbaas/pools/pool-1/members") != 1 {
		t.Fatalf("restarted provider did not rediscover pool: %#v, mutations %v", result, requests.mutationPaths())
	}
}

func TestReconcileGatewayGraphUsesClassHealthMonitorSettings(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPut && request.URL.Path == "/lbaas/healthmonitors/monitor-1" {
			requests.record(request)
			var body struct {
				Monitor map[string]any `json:"healthmonitor"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]any{"url_path": "/ready", "delay": float64(30), "timeout": float64(7), "max_retries": float64(4), "max_retries_down": float64(5)} {
				if got := body.Monitor[field]; got != want {
					t.Errorf("monitor.%s = %v, want %v", field, got, want)
				}
			}
			safetyWriteJSON(t, w, map[string]any{"healthmonitor": map[string]any{"id": "monitor-1"}})
			return
		}
		base.ServeHTTP(w, request)
	})
	spec := completeGraphSpec(identity)
	spec.Routes[0].Spec.HealthMonitor = cloud.HealthMonitorConfig{Path: "/ready", Delay: 30, Timeout: 7, MaxRetries: 4, MaxRetriesDown: 5}
	if _, err := completeGraphProvider(t, handler, requests).ReconcileGatewayGraph(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := requests.mutationPaths(); !reflect.DeepEqual(got, []string{"/lbaas/healthmonitors/monitor-1"}) {
		t.Fatalf("health monitor change mutated other resources: %v", got)
	}
}
