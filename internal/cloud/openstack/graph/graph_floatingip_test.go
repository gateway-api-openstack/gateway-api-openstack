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
)

func TestGatewayDeletionFindsDetachedFloatingIPAfterLoadBalancerDisappears(t *testing.T) {
	for _, getNotFound := range []bool{false, true} {
		name := "absent from discovery"
		if getNotFound {
			name = "disappeared after discovery"
		}
		t.Run(name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			remaining := true
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/floatingips":
					assertGatewayFloatingIPDiscoveryQuery(t, request)
					if request.URL.Query().Get("marker") == "" {
						safetyWriteJSON(t, w, map[string]any{
							"floatingips":       []any{map[string]string{"id": "foreign-fip", "project_id": "project-a", "description": "another controller", "port_id": "foreign-port"}},
							"floatingips_links": []map[string]string{{"rel": "next", "href": "http://openstack.test/floatingips?project_id=project-a&marker=foreign-fip"}},
						})
						return
					}
					items := []map[string]string{}
					if remaining {
						item := detachedGatewayFloatingIP(identity)
						item["description"] += "; controller-version=previous-release"
						items = append(items, item)
					}
					safetyWriteJSON(t, w, map[string]any{"floatingips": items})
				case request.Method == http.MethodDelete && request.URL.Path == "/floatingips/owned-fip":
					remaining = false
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Fatalf("unexpected Neutron request: %s %s", request.Method, request.URL)
				}
			})
			clients := ServiceClients{LoadBalancer: safetyServiceClient(absentGatewayHandler(t, identity, getNotFound)), Network: safetyServiceClient(network), ProjectID: "project-a"}
			spec := absentGatewaySpec(identity, cloud.GraphDelete)
			result, err := newTestProvider(t, clients, ProviderConfig{}).ReconcileGatewayGraph(context.Background(), spec)
			if err != nil || result.Outcome.State != cloud.OutcomeProgressing {
				t.Fatalf("first deletion = %#v, %v", result, err)
			}
			if got := requests.mutationPaths(); !reflect.DeepEqual(got, []string{"/floatingips/owned-fip"}) {
				t.Fatalf("mutations = %v, want only owned detached Floating IP deletion", got)
			}
			// A restarted adapter must confirm absence instead of retaining a plan.
			result, err = newTestProvider(t, clients, ProviderConfig{}).ReconcileGatewayGraph(context.Background(), spec)
			if err != nil || result.Outcome.State != cloud.OutcomeReady {
				t.Fatalf("confirmed deletion = %#v, %v", result, err)
			}
			if got := requests.count(http.MethodGet, "/floatingips"); got != 4 {
				t.Fatalf("Floating IP page reads = %d, want two pages for each observation", got)
			}
			if len(requests.mutationPaths()) != 1 {
				t.Fatal("confirmed absence performed another mutation")
			}
		})
	}
}

func TestAbsentGatewayRejectsConflictingFloatingIPBeforeMutation(t *testing.T) {
	identity := safetyTestIdentity(t)
	tests := []struct {
		name   string
		change func(map[string]string) []map[string]string
	}{
		{name: "duplicate identity", change: func(item map[string]string) []map[string]string {
			other := detachedGatewayFloatingIP(identity)
			other["id"] = "duplicate-fip"
			return []map[string]string{item, other}
		}},
		{name: "foreign project", change: func(item map[string]string) []map[string]string {
			item["project_id"] = "project-b"
			return []map[string]string{item}
		}},
		{name: "conflicting tenant", change: func(item map[string]string) []map[string]string {
			item["tenant_id"] = "project-b"
			return []map[string]string{item}
		}},
		{name: "missing resource ID", change: func(item map[string]string) []map[string]string {
			delete(item, "id")
			return []map[string]string{item}
		}},
		{name: "remaining port", change: func(item map[string]string) []map[string]string {
			item["port_id"] = "unexpected-port"
			return []map[string]string{item}
		}},
		{name: "remaining fixed address", change: func(item map[string]string) []map[string]string {
			item["fixed_ip_address"] = "192.0.2.10"
			return []map[string]string{item}
		}},
		{name: "remaining router", change: func(item map[string]string) []map[string]string {
			item["router_id"] = "unexpected-router"
			return []map[string]string{item}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
					t.Fatalf("mutation with conflicting ownership: %s %s", request.Method, request.URL)
				}
				assertGatewayFloatingIPDiscoveryQuery(t, request)
				safetyWriteJSON(t, w, map[string]any{"floatingips": test.change(detachedGatewayFloatingIP(identity))})
			})
			provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(absentGatewayHandler(t, identity, false)), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
			_, err := provider.ReconcileGatewayGraph(context.Background(), absentGatewaySpec(identity, cloud.GraphDelete))
			if !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("error = %v, want ownership conflict", err)
			}
		})
	}
}

func TestAbsentGatewayPreservesDetachedFloatingIPOutsideDeletion(t *testing.T) {
	for _, disposition := range []cloud.GraphDisposition{cloud.GraphPreserve, cloud.GraphEnsure} {
		t.Run(string(disposition), func(t *testing.T) {
			identity := safetyTestIdentity(t)
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
					t.Fatalf("unexpected mutation: %s %s", request.Method, request.URL)
				}
				assertGatewayFloatingIPDiscoveryQuery(t, request)
				safetyWriteJSON(t, w, map[string]any{"floatingips": []any{detachedGatewayFloatingIP(identity)}})
			})
			provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(absentGatewayHandler(t, identity, false)), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
			result, err := provider.ReconcileGatewayGraph(context.Background(), absentGatewaySpec(identity, disposition))
			if disposition == cloud.GraphEnsure {
				if !errors.Is(err, cloud.ErrOwnershipConflict) {
					t.Fatalf("replacement with orphan error = %v, want ownership conflict", err)
				}
			} else if err != nil || result.Outcome.State != cloud.OutcomeReady {
				t.Fatalf("preservation = %#v, %v", result, err)
			}
		})
	}
}

func TestGatewayDeletionDoesNotTreatIncompleteFloatingIPInventoryAsAbsent(t *testing.T) {
	identity := safetyTestIdentity(t)
	network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
			t.Fatalf("mutation before complete inventory: %s %s", request.Method, request.URL)
		}
		assertGatewayFloatingIPDiscoveryQuery(t, request)
		if request.URL.Query().Get("marker") != "" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		safetyWriteJSON(t, w, map[string]any{
			"floatingips":       []any{detachedGatewayFloatingIP(identity)},
			"floatingips_links": []map[string]string{{"rel": "next", "href": "http://openstack.test/floatingips?project_id=project-a&marker=owned-fip"}},
		})
	})
	provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(absentGatewayHandler(t, identity, false)), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
	_, err := provider.ReconcileGatewayGraph(context.Background(), absentGatewaySpec(identity, cloud.GraphDelete))
	if category, _ := cloud.ErrorCategoryOf(err); category != cloud.ErrorCategoryRetryableService {
		t.Fatalf("incomplete inventory error = %v, category %q", err, category)
	}
}

func TestGatewayGraphDetectsDetachedFloatingIPWithExistingLoadBalancer(t *testing.T) {
	for _, disposition := range []cloud.GraphDisposition{cloud.GraphEnsure, cloud.GraphPreserve, cloud.GraphDelete} {
		t.Run(string(disposition), func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				if request.Method == http.MethodDelete && request.URL.Path == "/floatingips/owned-fip" {
					if disposition != cloud.GraphDelete {
						t.Fatal("Floating IP deletion without Gateway deletion authority")
					}
					if requests.count(http.MethodGet, "/lbaas/pools") != 1 || requests.count(http.MethodGet, "/lbaas/listeners") != 1 {
						t.Fatal("deleted Floating IP before validating the surrounding graph")
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
					t.Fatalf("unexpected Floating IP allocation or reassociation: %s %s", request.Method, request.URL)
				}
				assertGatewayFloatingIPDiscoveryQuery(t, request)
				if request.URL.Query().Get("marker") == "" {
					safetyWriteJSON(t, w, map[string]any{
						"floatingips":       []any{map[string]string{"id": "foreign-fip", "project_id": "project-a", "description": "another controller", "port_id": "foreign-port"}},
						"floatingips_links": []map[string]string{{"rel": "next", "href": "http://openstack.test/floatingips?project_id=project-a&marker=foreign-fip"}},
					})
					return
				}
				item := detachedGatewayFloatingIP(identity)
				item["description"] += "; controller-version=previous-release"
				safetyWriteJSON(t, w, map[string]any{"floatingips": []any{item}})
			})
			provider := newTestProvider(t, ServiceClients{
				LoadBalancer: safetyServiceClient(gatewayWithoutChildrenHandler(t, identity, requests)),
				Network:      safetyServiceClient(network), ProjectID: "project-a",
			}, ProviderConfig{})
			spec := absentGatewaySpec(identity, disposition)
			spec.Gateway.Spec.ExternalNetworkID = "external-network"
			result, err := provider.ReconcileGatewayGraph(context.Background(), spec)
			if disposition == cloud.GraphEnsure {
				if !errors.Is(err, cloud.ErrOwnershipConflict) {
					t.Fatalf("detached Floating IP error = %v, want ownership conflict", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Gateway.State.FloatingIPAddress != "" {
				t.Fatal("detached Floating IP was reported as a usable Gateway address")
			}
			wantMutations := []string(nil)
			if disposition == cloud.GraphDelete {
				wantMutations = []string{"/floatingips/owned-fip"}
				if result.Outcome.State != cloud.OutcomeProgressing {
					t.Fatal("deletion reported absence before observing the Floating IP again")
				}
			}
			if got := requests.mutationPaths(); !reflect.DeepEqual(got, wantMutations) {
				t.Fatalf("mutations = %v, want %v", got, wantMutations)
			}
		})
	}
}

func TestGatewayGraphRejectsAttachedAndDetachedFloatingIPDuplicates(t *testing.T) {
	for _, disposition := range []cloud.GraphDisposition{cloud.GraphEnsure, cloud.GraphDelete} {
		t.Run(string(disposition), func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
					t.Fatalf("mutation with duplicate Floating IP identity: %s %s", request.Method, request.URL)
				}
				assertGatewayFloatingIPDiscoveryQuery(t, request)
				item := detachedGatewayFloatingIP(identity)
				if request.URL.Query().Get("marker") == "" {
					item["id"], item["port_id"] = "attached-fip", "vip-port"
					safetyWriteJSON(t, w, map[string]any{
						"floatingips":       []any{item},
						"floatingips_links": []map[string]string{{"rel": "next", "href": "http://openstack.test/floatingips?project_id=project-a&marker=attached-fip"}},
					})
					return
				}
				safetyWriteJSON(t, w, map[string]any{"floatingips": []any{item}})
			})
			provider := newTestProvider(t, ServiceClients{
				LoadBalancer: safetyServiceClient(gatewayWithoutChildrenHandler(t, identity, requests)),
				Network:      safetyServiceClient(network), ProjectID: "project-a",
			}, ProviderConfig{})
			_, err := provider.ReconcileGatewayGraph(context.Background(), absentGatewaySpec(identity, disposition))
			if !errors.Is(err, cloud.ErrOwnershipConflict) || len(requests.mutationPaths()) != 0 {
				t.Fatalf("duplicate inventory error = %v, mutations = %v", err, requests.mutationPaths())
			}
		})
	}
}

func gatewayWithoutChildrenHandler(t *testing.T, identity Identity, requests *gatewayTransitionRequests) http.Handler {
	t.Helper()
	lb := gatewayTransitionLoadBalancer(safetyGatewayTags(t, identity, roleLoadBalancer), "ACTIVE")
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected Octavia mutation: %s %s", request.Method, request.URL)
		}
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": []any{lb}})
		case "/lbaas/loadbalancers/load-balancer-1":
			safetyWriteJSON(t, w, map[string]any{"loadbalancer": lb})
		case "/lbaas/listeners":
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{}})
		case "/lbaas/pools":
			safetyWriteJSON(t, w, map[string]any{"pools": []any{}})
		default:
			t.Fatalf("unexpected Octavia observation: %s %s", request.Method, request.URL)
		}
	})
}

func absentGatewaySpec(identity Identity, disposition cloud.GraphDisposition) cloud.GatewayGraphSpec {
	id := gatewayIdentity(identity.Value())
	spec := gatewayTransitionSpec(identity)
	spec.Identity = id
	return cloud.GatewayGraphSpec{Identity: id, Gateway: cloud.GatewayGraphNode{Disposition: disposition, Spec: spec}}
}

func absentGatewayHandler(t *testing.T, identity Identity, getNotFound bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected Octavia mutation: %s %s", request.Method, request.URL)
		}
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			items := []map[string]any{}
			if getNotFound {
				items = append(items, map[string]any{"id": "missing-lb", "project_id": "project-a", "tags": safetyGatewayTags(t, identity, roleLoadBalancer)})
			}
			safetyWriteJSON(t, w, map[string]any{"loadbalancers": items})
		case "/lbaas/loadbalancers/missing-lb":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected Octavia request: %s %s", request.Method, request.URL)
		}
	})
}

func detachedGatewayFloatingIP(identity Identity) map[string]string {
	return map[string]string{"id": "owned-fip", "project_id": "project-a", "description": identity.GatewayDescription(roleFloatingIP), "floating_ip_address": "203.0.113.10"}
}

func assertGatewayFloatingIPDiscoveryQuery(t *testing.T, request *http.Request) {
	t.Helper()
	if request.URL.Query().Get("project_id") != "project-a" || request.URL.Query().Get("port_id") != "" || request.URL.Query().Get("description") != "" {
		t.Fatalf("Gateway Floating IP discovery filters = %s", request.URL.RawQuery)
	}
}
