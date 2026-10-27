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
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func TestBackendSecurityFailureRetainsRouteRetryAndRecovers(t *testing.T) {
	const serverID = "12345678-1234-1234-1234-123456789abc"
	identity := safetyTestIdentity(t)
	spec := completeGraphSpec(identity)
	spec.Routes[0].Spec.BackendSecurityGroupIDs = []string{"sg-a"}
	spec.Routes[0].Spec.Members[0].ProviderID = "openstack:///" + serverID
	requests := &gatewayTransitionRequests{}
	allowed := false
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.record(request)
		if request.Method == http.MethodPost && request.URL.Path == "/lbaas/loadbalancers" {
			if !allowed || requests.count(http.MethodGet, "/ports") == 0 {
				t.Fatal("created load balancer before validating backend access")
			}
			writer.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, writer, map[string]any{"loadbalancer": map[string]any{"id": "lb-1", "project_id": "project-a"}})
			return
		}
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected mutation: %s %s", request.Method, request.URL)
		}
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			safetyWriteJSON(t, writer, map[string]any{"loadbalancers": []any{}})
		case "/floatingips":
			safetyWriteJSON(t, writer, map[string]any{"floatingips": []any{}})
		case "/security-groups/sg-a":
			rules := []any{}
			if allowed {
				rules = append(rules, map[string]any{"direction": "ingress", "ethertype": "IPv4", "protocol": "tcp", "port_range_min": 30080, "port_range_max": 30080})
			}
			safetyWriteJSON(t, writer, map[string]any{"security_group": map[string]any{"id": "sg-a", "project_id": "project-a", "security_group_rules": rules}})
		case "/ports":
			safetyWriteJSON(t, writer, map[string]any{"ports": []any{map[string]any{
				"id": "worker-port", "project_id": "project-a", "device_id": serverID, "device_owner": "compute:nova",
				"admin_state_up": true, "port_security_enabled": true, "security_groups": []string{"sg-a", "foreign-group"},
				"fixed_ips": []any{map[string]any{"subnet_id": "member-subnet", "ip_address": "10.0.0.10"}},
			}}})
		default:
			t.Fatalf("unexpected observation: %s", request.URL)
		}
	})
	provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(handler), Network: safetyServiceClient(handler), ProjectID: "project-a"}, ProviderConfig{})
	_, err := provider.ReconcileGatewayGraph(context.Background(), spec)
	var component *cloud.GraphError
	category, _ := cloud.ErrorCategoryOf(err)
	if category != cloud.ErrorCategoryDependencyNotReady || !errors.As(err, &component) || component.Identity != spec.Routes[0].Spec.Identity {
		t.Fatalf("backend failure lost retry category or route identity: %v", err)
	}
	if len(requests.mutationPaths()) != 0 {
		t.Fatalf("mutations before backend validation: %v", requests.mutationPaths())
	}

	// Repair only the external rule. The same Kubernetes graph must resume on
	// its slow retry without a spec edit or another binding transition.
	allowed = true
	result, err := provider.ReconcileGatewayGraph(context.Background(), spec)
	if err != nil || result.Outcome.State != cloud.OutcomeProgressing || !reflect.DeepEqual(requests.mutationPaths(), []string{"/lbaas/loadbalancers"}) {
		t.Fatalf("did not recover after rule repair: result=%+v err=%v mutations=%v", result, err, requests.mutationPaths())
	}
	if requests.count(http.MethodGet, "/security-groups/sg-a") != 2 {
		t.Fatal("backend security rules were cached across reconciliations")
	}
}

func TestDesiredRouteRejectsConflictingMemberProviderIDs(t *testing.T) {
	identity := safetyTestIdentity(t)
	provider := newTestProvider(t, ServiceClients{ProjectID: "project-a"}, ProviderConfig{})
	for _, test := range []struct {
		name       string
		providerID string
		conflict   bool
	}{
		{name: "same instance", providerID: "openstack:///server-a"},
		{name: "different instance", providerID: "openstack:///server-b", conflict: true},
		{name: "missing identity", conflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := routeTransitionSpec(identity)
			spec.Members = []cloud.Member{
				{Address: "2001:db8::7", Port: 30080, ProviderID: "openstack:///server-a"},
				{Address: "2001:0db8::7", Port: 30080, ProviderID: test.providerID},
			}
			for range 2 {
				desired, err := provider.buildDesiredRoute(spec)
				if test.conflict {
					if !errors.Is(err, cloud.ErrTerminalValidation) {
						t.Fatalf("ambiguous member provider identity was accepted: %v", err)
					}
				} else if err != nil || len(desired.members) != 1 || desired.members[0].ProviderID != "openstack:///server-a" {
					t.Fatalf("identical normalized member tuples did not deduplicate: %v, %v", desired.members, err)
				}
				slices.Reverse(spec.Members)
			}
		})
	}
}

func TestRouteCleanupDoesNotRequireReferencedSecurityGroups(t *testing.T) {
	for _, test := range []struct {
		name     string
		children bool
	}{
		{name: "owned route still present", children: true},
		{name: "owned route already absent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			base := gatewayWithoutChildrenHandler(t, identity, requests)
			if test.children {
				base = convergedRouteGraphHandler(t, requests, identity, true, true)
			}
			handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1" {
					requests.record(request)
					loadBalancer := gatewayTransitionLoadBalancer(safetyGatewayTags(t, identity, roleLoadBalancer), "ACTIVE")
					loadBalancer["vip_sg_ids"] = []string{"deleted-frontend-group"}
					safetyWriteJSON(t, writer, map[string]any{"loadbalancer": loadBalancer})
					return
				}
				if request.Method == http.MethodPut && request.URL.Path == "/lbaas/l7policies/policy-1" {
					requests.record(request)
					var body struct {
						Policy struct {
							AdminStateUp *bool `json:"admin_state_up"`
						} `json:"l7policy"`
					}
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Policy.AdminStateUp == nil || *body.Policy.AdminStateUp {
						t.Fatalf("cleanup did not disable the policy: %+v, %v", body, err)
					}
					safetyWriteJSON(t, writer, map[string]any{"l7policy": map[string]any{"id": "policy-1"}})
					return
				}
				base.ServeHTTP(writer, request)
			})
			securityReads := 0
			network := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.HasPrefix(request.URL.Path, "/security-groups") || request.URL.Path == "/ports" {
					securityReads++
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				routeTransitionEmptyFloatingIPHandler(t, requests).ServeHTTP(writer, request)
			})
			provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(handler), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
			// The production DeleteRoute action preserves the Gateway and carries
			// only the immutable identity needed to delete its route fragment.
			result, err := provider.ReconcileGatewayGraph(context.Background(), cloud.GatewayGraphSpec{
				Identity: gatewayIdentity(identity.Value()), Gateway: cloud.GatewayGraphNode{Disposition: cloud.GraphPreserve},
				Routes: []cloud.RouteGraphNode{{Disposition: cloud.GraphDelete, Spec: cloud.RouteSpec{Identity: identity.Value()}}},
			})
			if err != nil || securityReads != 0 {
				t.Fatalf("cleanup depended on missing security groups: %v, reads=%d", err, securityReads)
			}
			if test.children {
				if result.Outcome.State != cloud.OutcomeProgressing || !reflect.DeepEqual(requests.mutationPaths(), []string{"/lbaas/l7policies/policy-1"}) {
					t.Fatalf("route cleanup did not progress: %+v, mutations=%v", result, requests.mutationPaths())
				}
			} else if result.Outcome.State != cloud.OutcomeReady || len(requests.mutationPaths()) != 0 {
				t.Fatalf("absent route was not ready to finalize: %+v, mutations=%v", result, requests.mutationPaths())
			}
		})
	}
}
