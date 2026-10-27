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

func TestGatewayGraphPreservesOrDeletesDetachedFloatingIPWithExistingLoadBalancer(t *testing.T) {
	for _, disposition := range []cloud.GraphDisposition{cloud.GraphPreserve, cloud.GraphDelete} {
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
			if err != nil {
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

func TestGatewayGraphRecoversDetachedFloatingIP(t *testing.T) {
	for _, lostResponse := range []bool{false, true} {
		name := "confirmed association"
		if lostResponse {
			name = "lost response"
		}
		t.Run(name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			item := recoverableGatewayFloatingIP(identity)
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				if serveFloatingIPRevisionExtension(t, w, request) {
					return
				}
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
					safetyWriteJSON(t, w, map[string]any{"floatingips": []any{item}})
				case request.Method == http.MethodPut && request.URL.Path == "/floatingips/owned-fip":
					if requests.count(http.MethodGet, "/lbaas/l7policies/policy-1/rules") != 1 {
						t.Fatal("restored Floating IP before observing the complete route graph")
					}
					var body map[string]map[string]any
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					want := map[string]map[string]any{"floatingip": {"port_id": "vip-port", "fixed_ip_address": "192.0.2.10"}}
					if !reflect.DeepEqual(body, want) || request.Header.Get("If-Match") != "revision_number=7" {
						t.Fatalf("association request = %#v, revision %q", body, request.Header.Get("If-Match"))
					}
					item["port_id"], item["fixed_ip_address"], item["router_id"] = "vip-port", "192.0.2.10", "router-1"
					item["revision_number"] = 8
					if lostResponse {
						http.Error(w, "response lost after update", http.StatusServiceUnavailable)
						return
					}
					safetyWriteJSON(t, w, map[string]any{"floatingip": item})
				default:
					t.Fatalf("unexpected Neutron request: %s %s", request.Method, request.URL)
				}
			})
			clients := ServiceClients{LoadBalancer: safetyServiceClient(convergedRouteGraphHandler(t, requests, identity, true, true)), Network: safetyServiceClient(network), ProjectID: "project-a"}
			spec := floatingIPGraphSpec(identity)
			result, err := newTestProvider(t, clients, ProviderConfig{}).ReconcileGatewayGraph(context.Background(), spec)
			if (err != nil) != lostResponse || result.Gateway.Outcome.State != cloud.OutcomeProgressing || result.Gateway.State.FloatingIPAddress != "" {
				t.Fatalf("association result = %#v, %v", result, err)
			}
			// The update response is not readiness evidence. A restarted provider
			// discovers the same address and performs no further mutation.
			result, err = newTestProvider(t, clients, ProviderConfig{}).ReconcileGatewayGraph(context.Background(), spec)
			if err != nil || result.Outcome.State != cloud.OutcomeReady || result.Gateway.State.FloatingIPID != "owned-fip" || result.Gateway.State.FloatingIPAddress != "203.0.113.10" {
				t.Fatalf("confirmed association = %#v, %v", result, err)
			}
			if got := requests.mutationPaths(); !reflect.DeepEqual(got, []string{"/floatingips/owned-fip"}) {
				t.Fatalf("mutations = %v, want one association without allocation or child changes", got)
			}
		})
	}
}

func TestGatewayGraphDoesNotRestoreFloatingIPBeforeSafeGraph(t *testing.T) {
	tests := []struct {
		name     string
		conflict bool
	}{
		{name: "preserved route"},
		{name: "deleted address with preserved route"},
		{name: "pending load balancer"},
		{name: "foreign pool", conflict: true},
		{name: "missing VIP port", conflict: true},
		{name: "different external network", conflict: true},
		{name: "foreign address on VIP", conflict: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			spec := floatingIPGraphSpec(identity)
			if test.name == "preserved route" || test.name == "deleted address with preserved route" {
				spec.Routes[0].Disposition = cloud.GraphPreserve
			}
			base := convergedRouteGraphHandler(t, requests, identity, true, true)
			octavia := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/lbaas/loadbalancers/load-balancer-1" && (test.name == "pending load balancer" || test.name == "missing VIP port") {
					requests.record(request)
					lb := gatewayTransitionLoadBalancer(safetyGatewayTags(t, identity, roleLoadBalancer), "ACTIVE")
					if test.name == "pending load balancer" {
						lb["provisioning_status"] = "PENDING_UPDATE"
					} else {
						delete(lb, "vip_port_id")
					}
					safetyWriteJSON(t, w, map[string]any{"loadbalancer": lb})
					return
				}
				if test.name == "foreign pool" && request.URL.Path == "/lbaas/pools" {
					requests.record(request)
					safetyWriteJSON(t, w, map[string]any{"pools": []any{map[string]any{"id": "foreign-pool", "tags": []string{}}}})
					return
				}
				base.ServeHTTP(w, request)
			})
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
					t.Fatalf("unexpected Neutron request before graph validation: %s %s", request.Method, request.URL)
				}
				item := recoverableGatewayFloatingIP(identity)
				if test.name == "different external network" {
					item["floating_network_id"] = "other-network"
				}
				items := []any{item}
				if test.name == "deleted address with preserved route" {
					items = nil
				}
				if test.name == "foreign address on VIP" {
					items = append(items, map[string]any{"id": "foreign-fip", "project_id": "project-a", "port_id": "vip-port", "fixed_ip_address": "192.0.2.10", "description": "another controller"})
				}
				safetyWriteJSON(t, w, map[string]any{"floatingips": items})
			})
			provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(octavia), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
			result, err := provider.ReconcileGatewayGraph(context.Background(), spec)
			if errors.Is(err, cloud.ErrOwnershipConflict) != test.conflict || !test.conflict && err != nil {
				t.Fatalf("reconcile error = %v, want conflict %t", err, test.conflict)
			}
			if !test.conflict && result.Gateway.Outcome.State != cloud.OutcomeProgressing {
				t.Fatalf("detached Gateway outcome = %#v, want Progressing", result.Gateway.Outcome)
			}
			if len(requests.mutationPaths()) != 0 || result.Gateway.State.FloatingIPAddress != "" {
				t.Fatalf("unsafe restoration: mutations %v, state %#v", requests.mutationPaths(), result.Gateway.State)
			}
		})
	}
}

func TestGatewayFloatingIPRecoveryRequiresConditionalUpdates(t *testing.T) {
	for _, alias := range []string{"standard-attr-revisions", "revision-if-match"} {
		t.Run(alias, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				if request.Method == http.MethodGet && request.URL.Path == "/extensions/"+alias {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if serveFloatingIPRevisionExtension(t, w, request) {
					return
				}
				if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
					t.Fatalf("recovery without revision support: %s %s", request.Method, request.URL)
				}
				safetyWriteJSON(t, w, map[string]any{"floatingips": []any{recoverableGatewayFloatingIP(identity)}})
			})
			provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(convergedRouteGraphHandler(t, requests, identity, true, true)), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
			_, err := provider.ReconcileGatewayGraph(context.Background(), floatingIPGraphSpec(identity))
			if category, _ := cloud.ErrorCategoryOf(err); category != cloud.ErrorCategoryDependencyNotReady || len(requests.mutationPaths()) != 0 {
				t.Fatalf("missing extension error = %v, mutations = %v", err, requests.mutationPaths())
			}
		})
	}
}

func TestGatewayFloatingIPRecoveryReobservesAfterRevisionConflict(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	item := recoverableGatewayFloatingIP(identity)
	network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		if serveFloatingIPRevisionExtension(t, w, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/floatingips":
			safetyWriteJSON(t, w, map[string]any{"floatingips": []any{item}})
		case request.Method == http.MethodPut && request.URL.Path == "/floatingips/owned-fip":
			item["port_id"], item["revision_number"] = "another-port", 8
			w.WriteHeader(http.StatusPreconditionFailed)
		default:
			t.Fatalf("unexpected Neutron request: %s %s", request.Method, request.URL)
		}
	})
	provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(convergedRouteGraphHandler(t, requests, identity, true, true)), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
	_, err := provider.ReconcileGatewayGraph(context.Background(), floatingIPGraphSpec(identity))
	if category, _ := cloud.ErrorCategoryOf(err); category != cloud.ErrorCategoryRetryableService {
		t.Fatalf("revision conflict category = %q, error %v", category, err)
	}
	_, err = provider.ReconcileGatewayGraph(context.Background(), floatingIPGraphSpec(identity))
	if !errors.Is(err, cloud.ErrOwnershipConflict) || len(requests.mutationPaths()) != 1 {
		t.Fatalf("reconcile after concurrent reassociation = %v, mutations %v", err, requests.mutationPaths())
	}
}

func TestGetGatewayReportsDetachedFloatingIPAsPending(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
			t.Fatalf("read-only Gateway lookup attempted %s %s", request.Method, request.URL)
		}
		safetyWriteJSON(t, w, map[string]any{"floatingips": []any{recoverableGatewayFloatingIP(identity)}})
	})
	provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(convergedRouteGraphHandler(t, requests, identity, true, true)), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
	result, found, err := provider.GetGateway(context.Background(), gatewayIdentity(identity.Value()))
	if err != nil || !found || result.Outcome.State != cloud.OutcomeProgressing || result.State.FloatingIPID != "owned-fip" || result.State.FloatingIPAddress != "" {
		t.Fatalf("detached Gateway lookup = %#v, %t, %v", result, found, err)
	}
}

func floatingIPGraphSpec(identity Identity) cloud.GatewayGraphSpec {
	spec := completeGraphSpec(identity)
	spec.Gateway.Spec.ExternalNetworkID = "external-network"
	spec.Routes[0].Spec.GatewayRequirements.ExternalNetworkID = "external-network"
	return spec
}

func recoverableGatewayFloatingIP(identity Identity) map[string]any {
	item := make(map[string]any)
	for key, value := range detachedGatewayFloatingIP(identity) {
		item[key] = value
	}
	item["description"] = identity.GatewayDescription(roleFloatingIP) + "; controller-version=previous-release"
	item["floating_network_id"], item["revision_number"] = "external-network", 7
	return item
}

func serveFloatingIPRevisionExtension(t *testing.T, w http.ResponseWriter, request *http.Request) bool {
	t.Helper()
	for _, alias := range []string{"standard-attr-revisions", "revision-if-match"} {
		if request.Method == http.MethodGet && request.URL.Path == "/extensions/"+alias {
			safetyWriteJSON(t, w, map[string]any{"extension": map[string]string{"alias": alias}})
			return true
		}
	}
	return false
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
					item["id"], item["port_id"], item["fixed_ip_address"] = "attached-fip", "vip-port", "192.0.2.10"
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

func TestGatewayFloatingIPRejectsWrongFixedAddress(t *testing.T) {
	for _, address := range []string{"", "192.0.2.20"} {
		t.Run("fixed address="+address, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			requests := &gatewayTransitionRequests{}
			network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.record(request)
				if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
					t.Fatalf("mutation with incorrect Floating IP association: %s %s", request.Method, request.URL)
				}
				item := recoverableGatewayFloatingIP(identity)
				item["port_id"], item["fixed_ip_address"] = "vip-port", address
				safetyWriteJSON(t, w, map[string]any{"floatingips": []any{item}})
			})
			provider := newTestProvider(t, ServiceClients{LoadBalancer: safetyServiceClient(convergedRouteGraphHandler(t, requests, identity, true, true)), Network: safetyServiceClient(network), ProjectID: "project-a"}, ProviderConfig{})
			if _, _, err := provider.GetGateway(t.Context(), gatewayIdentity(identity.Value())); !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("GetGateway error=%v", err)
			}
			for _, disposition := range []cloud.GraphDisposition{cloud.GraphEnsure, cloud.GraphDelete} {
				spec := floatingIPGraphSpec(identity)
				spec.Gateway.Disposition = disposition
				if disposition == cloud.GraphDelete {
					spec.Routes[0].Disposition = cloud.GraphDelete
				}
				if _, err := provider.ReconcileGatewayGraph(t.Context(), spec); !errors.Is(err, cloud.ErrOwnershipConflict) {
					t.Fatalf("%s error=%v", disposition, err)
				}
			}
			if len(requests.mutationPaths()) != 0 {
				t.Fatalf("incorrect association allowed mutations: %v", requests.mutationPaths())
			}
		})
	}
}

func TestGatewayBootstrapAllocatesFloatingIPAfterListener(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	listenerExists := false
	base := gatewayWithoutChildrenHandler(t, identity, requests)
	octavia := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/lbaas/listeners":
			requests.record(request)
			listenerExists = true
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"listener": map[string]any{"id": "listener-1", "project_id": "project-a"}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/listeners" && listenerExists:
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"listeners": []any{map[string]any{
				"id": "listener-1", "project_id": "project-a", "protocol": "HTTP", "protocol_port": 80,
				"admin_state_up": true, "tags": safetyGatewayTags(t, identity, roleListener),
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/lbaas/l7policies":
			requests.record(request)
			safetyWriteJSON(t, w, map[string]any{"l7policies": []any{}})
		default:
			base.ServeHTTP(w, request)
		}
	})
	var allocated map[string]any
	network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/floatingips":
			items := []any{}
			if allocated != nil {
				items = append(items, allocated)
			}
			safetyWriteJSON(t, w, map[string]any{"floatingips": items})
		case request.Method == http.MethodPost && request.URL.Path == "/floatingips":
			var body map[string]map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !listenerExists || body["floatingip"]["port_id"] != "vip-port" || body["floatingip"]["fixed_ip_address"] != "192.0.2.10" {
				t.Fatalf("allocation preceded listener validation or used another VIP: %#v", body)
			}
			allocated = body["floatingip"]
			allocated["id"], allocated["project_id"], allocated["floating_ip_address"] = "owned-fip", "project-a", "203.0.113.10"
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"floatingip": allocated})
		default:
			t.Fatalf("unexpected allocation request: %s %s", request.Method, request.URL)
		}
	})
	clients := ServiceClients{LoadBalancer: safetyServiceClient(octavia), Network: safetyServiceClient(network), ProjectID: "project-a"}
	spec := absentGatewaySpec(identity, cloud.GraphEnsure)
	spec.Gateway.Spec.ExternalNetworkID = "external-network"
	for attempt := range 3 {
		result, err := newTestProvider(t, clients, ProviderConfig{}).ReconcileGatewayGraph(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 2 {
			if result.Outcome.State != cloud.OutcomeProgressing || len(requests.mutationPaths()) != attempt+1 || result.Gateway.State.FloatingIPAddress != "" {
				t.Fatalf("allocation step %d result=%#v, mutations=%v", attempt, result, requests.mutationPaths())
			}
		} else if result.Outcome.State != cloud.OutcomeReady || result.Gateway.State.FloatingIPAddress != "203.0.113.10" {
			t.Fatalf("observed allocated address=%#v", result)
		}
	}
	if !reflect.DeepEqual(requests.mutationPaths(), []string{"/lbaas/listeners", "/floatingips"}) {
		t.Fatalf("bootstrap mutations=%v", requests.mutationPaths())
	}
}

func TestFloatingIPQuotaRecoveryObservesBeforeAllocatingAgain(t *testing.T) {
	identity := safetyTestIdentity(t)
	requests := &gatewayTransitionRequests{}
	quotaBlocked := true
	var allocated map[string]any
	attempts := 0
	network := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.record(request)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/floatingips":
			assertGatewayFloatingIPDiscoveryQuery(t, request)
			items := []any{}
			if allocated != nil {
				items = append(items, allocated)
			}
			safetyWriteJSON(t, w, map[string]any{"floatingips": items})
		case request.Method == http.MethodPost && request.URL.Path == "/floatingips":
			attempts++
			if allocated != nil {
				t.Fatal("allocated another Floating IP without discovering the previous result")
			}
			if quotaBlocked {
				w.WriteHeader(http.StatusConflict)
				safetyWriteJSON(t, w, map[string]any{"NeutronError": map[string]any{"type": "OverQuota", "message": "private quota details"}})
				return
			}
			var body struct {
				FloatingIP map[string]any `json:"floatingip"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			allocated = body.FloatingIP
			if allocated["port_id"] != "vip-port" || allocated["floating_network_id"] != "external-network" || allocated["description"] != identity.GatewayDescription(roleFloatingIP) {
				t.Fatalf("recovery lost the frozen address identity: %#v", allocated)
			}
			allocated["id"], allocated["project_id"] = "replacement-fip", "project-a"
			allocated["fixed_ip_address"], allocated["floating_ip_address"] = "192.0.2.10", "203.0.113.20"
			w.WriteHeader(http.StatusCreated)
			safetyWriteJSON(t, w, map[string]any{"floatingip": allocated})
		default:
			t.Fatalf("unexpected network mutation during quota recovery: %s %s", request.Method, request.URL)
		}
	})
	clients := ServiceClients{LoadBalancer: safetyServiceClient(convergedRouteGraphHandler(t, requests, identity, true, true)), Network: safetyServiceClient(network), ProjectID: "project-a"}
	provider := newTestProvider(t, clients, ProviderConfig{})
	spec := floatingIPGraphSpec(identity)
	for retry := range 2 {
		_, err := provider.ReconcileGatewayGraph(t.Context(), spec)
		if !errors.Is(err, cloud.ErrQuotaExceeded) || attempts != retry+1 || allocated != nil {
			t.Fatalf("quota failure = %v, attempts=%d allocated=%v", err, attempts, allocated != nil)
		}
	}
	// Changing only the API response models quota repair. It does not consume
	// real quota or require a Kubernetes edit to resume the same desired graph.
	quotaBlocked = false
	result, err := provider.ReconcileGatewayGraph(t.Context(), spec)
	if err != nil || result.Outcome.State != cloud.OutcomeProgressing || attempts != 3 {
		t.Fatalf("quota recovery = %+v, %v, attempts=%d", result, err, attempts)
	}
	// A fresh adapter must rediscover the successful allocation. It cannot rely
	// on a process-local checkpoint to avoid allocating a duplicate.
	result, err = newTestProvider(t, clients, ProviderConfig{}).ReconcileGatewayGraph(t.Context(), spec)
	if err != nil || result.Outcome.State != cloud.OutcomeReady || attempts != 3 || result.Gateway.State.FloatingIPID != "replacement-fip" {
		t.Fatalf("converged recovery = %+v, %v, attempts=%d", result, err, attempts)
	}
	if got := requests.mutationPaths(); !reflect.DeepEqual(got, []string{"/floatingips", "/floatingips", "/floatingips"}) {
		t.Fatalf("quota recovery changed another resource: %v", got)
	}
}
