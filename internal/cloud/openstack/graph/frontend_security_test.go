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
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/octavia"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
)

func TestGatewayGraphCreatesOnlyAfterReferencedFrontendValidation(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		name := "listener traffic denied"
		if allowed {
			name = "listener traffic allowed"
		}
		t.Run(name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				if request.Method == http.MethodPost && request.URL.Path == "/lbaas/loadbalancers" {
					if !allowed || requests.count(http.MethodGet, "/security-groups/sg-a") != 1 {
						t.Fatal("created a load balancer before validating its referenced security group")
					}
					var body struct {
						LoadBalancer map[string]any `json:"loadbalancer"`
					}
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(body.LoadBalancer["vip_sg_ids"], []any{"sg-a"}) {
						t.Fatalf("created load balancer without referenced security groups: %#v", body)
					}
					w.WriteHeader(http.StatusCreated)
					safetyWriteJSON(t, w, map[string]any{"loadbalancer": map[string]any{"id": "lb-1", "project_id": "project-a"}})
					return
				}
				if request.Method != http.MethodGet {
					t.Fatalf("unexpected mutation: %s %s", request.Method, request.URL)
				}
				switch request.URL.Path {
				case "/":
					safetyWriteJSON(t, w, map[string]any{"versions": []any{map[string]string{"id": "v2.29", "status": "CURRENT"}}})
				case "/lbaas/providers":
					safetyWriteJSON(t, w, map[string]any{"providers": []any{map[string]string{"name": "amphora"}}})
				case "/lbaas/loadbalancers":
					safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{}})
				case "/floatingips":
					safetyWriteJSON(t, w, map[string]any{"floatingips": []any{}})
				case "/subnets/vip-subnet":
					safetyWriteJSON(t, w, map[string]any{"subnet": map[string]any{"id": "vip-subnet", "network_id": "private-net", "project_id": "project-a", "cidr": "192.0.2.0/24", "ip_version": 4}})
				case "/security-groups/sg-a":
					rules := []any{}
					if allowed {
						rules = append(rules, map[string]any{"direction": "ingress", "ethertype": "IPv4", "protocol": "tcp", "port_range_min": 80, "port_range_max": 80})
					}
					safetyWriteJSON(t, w, map[string]any{"security_group": map[string]any{"id": "sg-a", "project_id": "project-a", "security_group_rules": rules}})
				default:
					t.Fatalf("unexpected observation: %s", request.URL)
				}
			})
			octaviaClient := safetyServiceClient(handler)
			octaviaClient.Microversion = "2.29"
			provider := newTestProvider(t, ServiceClients{LoadBalancer: octaviaClient, Network: safetyServiceClient(handler), ProjectID: "project-a"}, ProviderConfig{})
			spec := absentGatewaySpec(identity, cloud.GraphEnsure)
			spec.Gateway.Spec.VIPSecurityGroupIDs = []string{"sg-a"}
			result, err := provider.ReconcileGatewayGraph(context.Background(), spec)
			if allowed {
				if err != nil || result.Outcome.State != cloud.OutcomeProgressing || !reflect.DeepEqual(requests.mutationPaths(), []string{"/lbaas/loadbalancers"}) {
					t.Fatalf("creation = %#v, %v, mutations %v", result, err, requests.mutationPaths())
				}
			} else if category, _ := cloud.ErrorCategoryOf(err); category != cloud.ErrorCategoryDependencyNotReady || len(requests.mutationPaths()) != 0 {
				t.Fatalf("invalid rules error = %v, mutations %v", err, requests.mutationPaths())
			}
		})
	}
}

func TestGraphLoadBalancerValidatesExactVIPSecurityGroups(t *testing.T) {
	tests := []struct {
		name     string
		actual   []string
		wanted   []string
		conflict bool
	}{
		{name: "Octavia managed"},
		{name: "order is not drift", actual: []string{"sg-b", "sg-a"}, wanted: []string{"sg-a", "sg-b"}},
		{name: "missing reference", wanted: []string{"sg-a"}, conflict: true},
		{name: "unexpected custom group", actual: []string{"sg-a"}, conflict: true},
		{name: "changed reference", actual: []string{"sg-b"}, wanted: []string{"sg-a"}, conflict: true},
		{name: "duplicate group", actual: []string{"sg-a", "sg-a"}, wanted: []string{"sg-a", "sg-b"}, conflict: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lb := &octavia.LoadBalancer{LoadBalancer: loadbalancers.LoadBalancer{Provider: "amphora", VipSubnetID: "vip-subnet"}, VIPSecurityGroupIDs: test.actual}
			before := append([]string(nil), test.actual...)
			err := validateGraphLoadBalancer(lb, cloud.GatewayRequirements{Provider: "amphora", VIPSubnetID: "vip-subnet", VIPSecurityGroupIDs: test.wanted})
			if errors.Is(err, cloud.ErrOwnershipConflict) != test.conflict {
				t.Fatalf("validation error = %v, want conflict %t", err, test.conflict)
			}
			if !reflect.DeepEqual(lb.VIPSecurityGroupIDs, before) {
				t.Fatal("validation changed the observed security group order")
			}
		})
	}
}

func TestGatewayGraphRejectsVIPSecurityConflictBeforeMutation(t *testing.T) {
	for _, allowedCIDRs := range []bool{false, true} {
		name := "unexpected security group"
		if allowedCIDRs {
			name = "incompatible listener allowed CIDRs"
		}
		t.Run(name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			base := gatewayWithoutChildrenHandler(t, identity, requests)
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1" {
					requests.record(request)
					lb := gatewayTransitionLoadBalancer(safetyGatewayTags(t, identity, roleLoadBalancer), "ACTIVE")
					lb["vip_sg_ids"] = []string{"sg-a"}
					safetyWriteJSON(t, w, map[string]any{"loadbalancer": lb})
					return
				}
				if allowedCIDRs && request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners" {
					requests.record(request)
					safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
						"id": "listener-1", "protocol": "HTTP", "protocol_port": 80, "admin_state_up": true,
						"tags": safetyGatewayTags(t, identity, roleListener), "allowed_cidrs": []string{"192.0.2.0/24"},
					}}})
					return
				}
				base.ServeHTTP(w, request)
			})
			spec := absentGatewaySpec(identity, cloud.GraphEnsure)
			if allowedCIDRs {
				spec.Gateway.Spec.VIPSecurityGroupIDs = []string{"sg-a"}
			}
			provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(handler), ProjectID: "project-a"}, ProviderConfig{})
			_, err := provider.ReconcileGatewayGraph(context.Background(), spec)
			if !errors.Is(err, cloud.ErrOwnershipConflict) || len(requests.mutationPaths()) != 0 {
				t.Fatalf("security conflict error = %v, mutations = %v", err, requests.mutationPaths())
			}
		})
	}
}

func TestFrontendRuleRepairResumesExistingGraphWithoutMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	allowed := false
	base := convergedRouteGraphHandler(t, requests, identity, true, true)
	loadBalancer := gatewayTransitionLoadBalancer(safetyGatewayTags(t, identity, roleLoadBalancer), "ACTIVE")
	loadBalancer["vip_sg_ids"] = []string{"sg-a"}
	octaviaHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("security repair attempted to mutate an existing graph: %s %s", request.Method, request.URL)
		}
		switch request.URL.Path {
		case "/":
			safetyWriteJSON(t, w, map[string]any{"versions": []any{map[string]string{"id": "v2.29", "status": "CURRENT"}}})
		case "/lbaas/providers":
			safetyWriteJSON(t, w, map[string]any{"providers": []any{map[string]string{"name": "amphora"}}})
		case "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{loadBalancer}})
		case "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": loadBalancer})
		default:
			base.ServeHTTP(w, request)
		}
	})
	groupReads := 0
	network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("security repair attempted to change a referenced dependency: %s %s", request.Method, request.URL)
		}
		switch request.URL.Path {
		case "/floatingips":
			safetyWriteJSON(t, w, map[string]any{"floatingips": []any{}})
		case "/subnets/vip-subnet":
			safetyWriteJSON(t, w, map[string]any{"subnet": map[string]any{"id": "vip-subnet", "network_id": "private-net", "project_id": "project-a", "cidr": "192.0.2.0/24", "ip_version": 4}})
		case "/security-groups/sg-a":
			groupReads++
			rules := []any{}
			if allowed {
				rules = append(rules, map[string]any{"direction": "ingress", "ethertype": "IPv4", "protocol": "tcp", "port_range_min": 80, "port_range_max": 80})
			}
			safetyWriteJSON(t, w, map[string]any{"security_group": map[string]any{"id": "sg-a", "project_id": "project-a", "security_group_rules": rules}})
		default:
			t.Fatalf("unexpected security observation: %s", request.URL)
		}
	})
	lbClient := safetyServiceClient(octaviaHandler)
	lbClient.Microversion = "2.29"
	provider := newTestProvider(t, ServiceClients{LoadBalancer: lbClient, Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
	spec := completeGraphSpec(identity)
	spec.Gateway.Spec.VIPSecurityGroupIDs = []string{"sg-a"}
	spec.Routes[0].Spec.GatewayRequirements.VIPSecurityGroupIDs = []string{"sg-a"}
	if _, err := provider.ReconcileGatewayGraph(t.Context(), spec); !errors.Is(err, cloud.ErrDependencyNotReady) {
		t.Fatalf("missing rule failure = %v", err)
	}
	// Only the external group's observed rules change. No Kubernetes intent,
	// frozen binding, or controller process changes to trigger recovery.
	allowed = true
	result, err := provider.ReconcileGatewayGraph(t.Context(), spec)
	if err != nil || result.Outcome.State != cloud.OutcomeReady || groupReads != 2 {
		t.Fatalf("same-graph rule recovery = %+v, %v, group reads=%d", result, err, groupReads)
	}
}
