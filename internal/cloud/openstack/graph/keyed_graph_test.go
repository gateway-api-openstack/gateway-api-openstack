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
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	openstackidentity "github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/identity"
)

func TestKeyedGraphPreservesUnchangedListenersAndRules(t *testing.T) {
	identity := safetyTestIdentity(t)
	spec := completeGraphSpec(identity)
	spec.Routes[0].Spec.Gateway = cloud.GatewayState{}
	first := cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 80}
	second := cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 8080}
	spec.Gateway.Spec.Listeners = []cloud.ListenerSpec{{Key: first}, {Key: second}}
	rule := &spec.Routes[0].Spec.Rules[0]
	rule.Matches = []cloud.RouteMatchSpec{
		{Listener: first, Priority: 1, Hostname: "api.example.test", PathType: cloud.PathMatchExact, PathValue: "/api", Headers: []cloud.HeaderMatch{{Name: "x-stage", Value: "blue"}}},
		{Listener: first, Priority: 2, HostIndex: 1, Hostname: "*.example.test", PathType: cloud.PathMatchExact, PathValue: "/api"},
		{Listener: second, Priority: 0, PathType: cloud.PathMatchPrefix, PathValue: "/"},
	}
	// A noncontiguous slot must remain removable after it leaves the spec.
	spec.Routes[0].Spec.Rules = append(spec.Routes[0].Spec.Rules, cloud.RouteRuleSpec{
		Index: 3, Members: []cloud.Member{{Address: "10.0.0.20", Port: 30080}},
		Matches: []cloud.RouteMatchSpec{{Listener: first, Priority: 0, PathType: cloud.PathMatchPrefix, PathValue: "/other"}},
	})
	server := newKeyedGraphServer(t, identity)
	convergeKeyedGraph(t, server, spec)
	before := server.ids()
	listenerID := server.find("listeners", func(item map[string]any) bool { return item["protocol_port"] == float64(80) })
	poolID := server.findKey("pools", "rule/0")
	memberID := server.find("members", func(item map[string]any) bool { return item["pool_id"] == poolID })
	if listenerID == "" || poolID == "" || memberID == "" {
		t.Fatalf("missing initial resources: %v", before)
	}
	// Keep one host alternative and change its predicate while retaining its slot.
	spec.Gateway.Spec.Listeners = spec.Gateway.Spec.Listeners[:1]
	spec.Routes[0].Spec.Rules = spec.Routes[0].Spec.Rules[:1]
	spec.Routes[0].Spec.Rules[0].Matches = spec.Routes[0].Spec.Rules[0].Matches[:1]
	spec.Routes[0].Spec.Rules[0].Matches[0].Hostname = "new.example.test"
	spec.Routes[0].Spec.Rules[0].Matches[0].Headers = []cloud.HeaderMatch{{Name: "x-stage", Value: "green"}}
	convergeKeyedGraph(t, server, spec)
	if len(server.items["listeners"]) != 1 || server.items["listeners"][listenerID] == nil || len(server.items["pools"]) != 1 || server.items["pools"][poolID] == nil || server.items["members"][memberID] == nil {
		t.Fatalf("survivor identity changed: before=%v after=%v", before, server.ids())
	}
	if len(server.items["l7policies"]) != 1 || len(server.items["rules"]) != 3 {
		t.Fatalf("obsolete match resources remain: %v", server.ids())
	}
	for range 3 {
		convergeKeyedGraph(t, server, spec)
	}
	// Gateway finalization discovers child ownership without a Route object.
	spec.Gateway.Disposition = cloud.GraphDelete
	spec.Routes = nil
	convergeKeyedGraph(t, server, spec)
	for resource, items := range server.items {
		if len(items) != 0 {
			t.Fatalf("%s remains after Gateway deletion: %v", resource, items)
		}
	}
}

func TestKeyedGraphRejectsMisplacedAndDuplicateChildrenBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*keyedGraphServer)
	}{
		{name: "duplicate pool slot", alter: func(s *keyedGraphServer) {
			for _, pool := range s.items["pools"] {
				duplicate := cloneTestMap(pool)
				duplicate["id"] = "duplicate"
				s.items["pools"]["duplicate"] = duplicate
				break
			}
		}},
		{name: "member in wrong rule", alter: func(s *keyedGraphServer) {
			for _, member := range s.items["members"] {
				replaceTestChildKey(member, "rule/9")
				break
			}
		}},
		{name: "predicate in wrong policy", alter: func(s *keyedGraphServer) {
			for _, rule := range s.items["rules"] {
				key, _ := testStoredKey(rule)
				key.MatchIndex = 9
				replaceTestChildKey(rule, key.String())
				break
			}
		}},
		{name: "policy role differs from key", alter: func(s *keyedGraphServer) {
			for _, policy := range s.items["l7policies"] {
				key, _ := testStoredKey(policy)
				if key.Variant == "exact" {
					key.Variant = "prefix"
				} else {
					key.Variant = "exact"
				}
				replaceTestChildKey(policy, key.String())
				break
			}
		}},
		{name: "predicate role differs from key", alter: func(s *keyedGraphServer) {
			for _, rule := range s.items["rules"] {
				key, _ := testStoredKey(rule)
				if key.Predicate == "path" {
					key.Predicate = "host"
				} else {
					key.Predicate = "path"
				}
				replaceTestChildKey(rule, key.String())
				break
			}
		}},
		{name: "missing key", alter: func(s *keyedGraphServer) {
			for _, pool := range s.items["pools"] {
				tags := pool["tags"].([]any)
				pool["tags"] = tags[:len(tags)-1]
				break
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := safetyTestIdentity(t)
			spec := completeGraphSpec(identity)
			spec.Routes[0].Spec.Gateway = cloud.GatewayState{}
			server := newKeyedGraphServer(t, identity)
			convergeKeyedGraph(t, server, spec)
			test.alter(server)
			before := server.mutations
			_, err := newTestProvider(t, ServiceClients{ProjectID: "project-a", LoadBalancer: safetyServiceClient(server)}, ProviderConfig{}).ReconcileGatewayGraph(context.Background(), spec)
			if !errors.Is(err, cloud.ErrOwnershipConflict) || server.mutations != before {
				t.Fatalf("unsafe graph: err=%v mutations=%d", err, server.mutations-before)
			}
			spec.Gateway.Disposition = cloud.GraphDelete
			spec.Routes = nil
			_, err = newTestProvider(t, ServiceClients{ProjectID: "project-a", LoadBalancer: safetyServiceClient(server)}, ProviderConfig{}).ReconcileGatewayGraph(context.Background(), spec)
			if !errors.Is(err, cloud.ErrOwnershipConflict) || server.mutations != before {
				t.Fatalf("unsafe finalization: err=%v mutations=%d", err, server.mutations-before)
			}
		})
	}
}

func TestHostnamePredicatesIgnorePortAndPreserveWildcardBoundary(t *testing.T) {
	for _, tt := range []struct {
		hostname string
		matches  []string
		misses   []string
	}{
		{
			hostname: "example.test",
			matches:  []string{"example.test", "EXAMPLE.TEST", "example.test:80", "EXAMPLE.test:8080"},
			misses:   []string{"api.example.test", "example.test.evil", "notexample.test", "example.test:abc"},
		},
		{
			hostname: "*.example.test",
			matches:  []string{"api.example.test", "API.Example.Test", "api.example.test:443", "deep.api.example.test:8080"},
			misses:   []string{"example.test", "example.test:443", "badexample.test", "api.example.test.evil", "api.example.test:abc", ".example.test", "a..example.test"},
		},
	} {
		t.Run(tt.hostname, func(t *testing.T) {
			match := cloud.RouteMatchSpec{Listener: cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 80}, Hostname: tt.hostname, PathType: cloud.PathMatchExact, PathValue: "/"}
			policies, err := policiesFor(cloud.RouteRuleSpec{Matches: []cloud.RouteMatchSpec{match}})
			if err != nil {
				t.Fatal(err)
			}
			rules := rulesFor(policies[0])
			if len(rules) != 2 || rules[1].ruleType != "HOST_NAME" || rules[1].compareType != "REGEX" {
				t.Fatalf("hostname predicate = %#v", rules)
			}
			// Octavia renders HOST_NAME ACLs with HAProxy's -i flag. Fold
			// these ASCII DNS names while checking portable POSIX syntax.
			expression, err := regexp.CompilePOSIX(rules[1].value)
			if err != nil {
				t.Fatal(err)
			}
			for _, host := range tt.matches {
				if !expression.MatchString(strings.ToLower(host)) {
					t.Errorf("hostname %q did not match %q", host, rules[1].value)
				}
			}
			for _, host := range tt.misses {
				if expression.MatchString(strings.ToLower(host)) {
					t.Errorf("hostname %q unexpectedly matched %q", host, rules[1].value)
				}
			}
		})
	}
}

func convergeKeyedGraph(t *testing.T, server *keyedGraphServer, spec cloud.GatewayGraphSpec) {
	t.Helper()
	for range 160 {
		before := server.mutations
		// Reconstruct the provider on every pass to exercise durable discovery.
		provider := newTestProvider(t, ServiceClients{ProjectID: "project-a", LoadBalancer: safetyServiceClient(server)}, ProviderConfig{})
		result, err := provider.ReconcileGatewayGraph(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		changes := server.mutations - before
		if changes > 1 {
			t.Fatalf("one reconcile issued %d mutations", changes)
		}
		if result.Outcome.State == cloud.OutcomeReady {
			if changes != 0 {
				t.Fatal("ready result mutated cloud resources")
			}
			return
		}
		if changes == 0 {
			t.Fatalf("graph made no progress: %#v", result)
		}
	}
	t.Fatalf("graph did not converge: %v", server.ids())
}

// keyedGraphServer models Octavia's parent links and policy renumbering. It is
// intentionally confined to the HTTP graph paths exercised in these tests.
type keyedGraphServer struct {
	t         *testing.T
	items     map[string]map[string]map[string]any
	mutations int
	nextID    int
}

func newKeyedGraphServer(t *testing.T, identity Identity) *keyedGraphServer {
	t.Helper()
	s := &keyedGraphServer{t: t, items: map[string]map[string]map[string]any{}}
	for _, kind := range []string{"loadbalancers", "listeners", "pools", "members", "healthmonitors", "l7policies", "rules"} {
		s.items[kind] = map[string]map[string]any{}
	}
	s.items["loadbalancers"]["load-balancer-1"] = gatewayTransitionLoadBalancer(safetyGatewayTags(t, identity, roleLoadBalancer), "ACTIVE")
	return s
}

func (s *keyedGraphServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/lbaas/"), "/")
	kind, id, parentField, parentID := parts[0], "", "", ""
	if len(parts) > 1 {
		id = parts[1]
	}
	if len(parts) >= 3 {
		parentID = id
		kind = parts[2]
		id = ""
		if parts[0] == "pools" {
			parentField = "pool_id"
		} else {
			parentField = "l7policy_id"
		}
		if len(parts) == 4 {
			id = parts[3]
		}
	}
	collection := s.items[kind]
	if collection == nil {
		s.t.Fatalf("unexpected request %s %s", r.Method, r.URL)
	}
	singular := strings.TrimSuffix(kind, "s")
	if kind == "l7policies" {
		singular = "l7policy"
	}
	if r.Method == http.MethodGet {
		if id != "" {
			item := collection[id]
			if item == nil {
				http.NotFound(w, r)
				return
			}
			safetyWriteJSON(s.t, w, map[string]any{singular: item})
			return
		}
		values := []map[string]any{}
		for _, item := range collection {
			if parentField != "" && item[parentField] != parentID {
				continue
			}
			if listener := r.URL.Query().Get("listener_id"); listener != "" && item["listener_id"] != listener {
				continue
			}
			if pool := r.URL.Query().Get("pool_id"); pool != "" && item["pool_id"] != pool {
				continue
			}
			values = append(values, item)
		}
		sort.Slice(values, func(i, j int) bool { return values[i]["id"].(string) < values[j]["id"].(string) })
		safetyWriteJSON(s.t, w, map[string]any{kind: values})
		return
	}
	s.mutations++
	if r.Method == http.MethodDelete {
		item := collection[id]
		delete(collection, id)
		if kind == "l7policies" && item != nil {
			s.renumber(item["listener_id"].(string), "", 0)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body map[string]map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.t.Fatal(err)
	}
	payload := body[singular]
	if payload == nil {
		s.t.Fatalf("missing %s body", singular)
	}
	if kind == "rules" {
		policy := s.items["l7policies"][parentID]
		if policy["admin_state_up"] != false {
			s.t.Fatal("changed a match while its policy was enabled")
		}
	}
	if r.Method == http.MethodPost {
		s.nextID++
		id = fmt.Sprintf("%s-%d", kind, s.nextID)
		collection[id] = map[string]any{"id": id, "project_id": "project-a"}
	}
	item := collection[id]
	if item == nil {
		s.t.Fatalf("update missing %s", id)
	}
	for key, value := range payload {
		item[key] = value
	}
	if parentField != "" {
		item[parentField] = parentID
	}
	if kind == "listeners" || kind == "pools" {
		item["loadbalancers"] = []map[string]any{{"id": "load-balancer-1"}}
	}
	if kind == "healthmonitors" {
		item["pools"] = []map[string]any{{"id": item["pool_id"]}}
	}
	if kind == "l7policies" {
		if position, ok := payload["position"].(float64); ok {
			s.renumber(item["listener_id"].(string), id, int(position))
		}
	}
	if r.Method == http.MethodPost {
		w.WriteHeader(http.StatusCreated)
	}
	safetyWriteJSON(s.t, w, map[string]any{singular: item})
}

func (s *keyedGraphServer) renumber(listener, moved string, position int) {
	var policies []map[string]any
	for id, item := range s.items["l7policies"] {
		if item["listener_id"] == listener && id != moved {
			policies = append(policies, item)
		}
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i]["position"].(float64) < policies[j]["position"].(float64) })
	if moved != "" {
		position = max(1, min(position, len(policies)+1))
		policies = slices.Insert(policies, position-1, s.items["l7policies"][moved])
	}
	for index, item := range policies {
		item["position"] = float64(index + 1)
	}
}

func (s *keyedGraphServer) ids() map[string][]string {
	result := map[string][]string{}
	for kind, items := range s.items {
		for id := range items {
			result[kind] = append(result[kind], id)
		}
		sort.Strings(result[kind])
	}
	return result
}
func (s *keyedGraphServer) find(kind string, matches func(map[string]any) bool) string {
	for id, item := range s.items[kind] {
		if matches(item) {
			return id
		}
	}
	return ""
}
func (s *keyedGraphServer) findKey(kind, key string) string {
	return s.find(kind, func(item map[string]any) bool { k, err := testStoredKey(item); return err == nil && k.String() == key })
}
func testStoredKey(item map[string]any) (cloud.ResourceKey, error) {
	var tags []string
	for _, tag := range item["tags"].([]any) {
		tags = append(tags, tag.(string))
	}
	return openstackidentity.ChildKey(tags)
}
func replaceTestChildKey(item map[string]any, key string) {
	tags := item["tags"].([]any)
	for index, tag := range tags {
		if strings.HasPrefix(tag.(string), "gateway-api-openstack.resource-key=") {
			tags[index] = openstackidentity.Tag("resource-key", key)
		}
	}
}
func cloneTestMap(item map[string]any) map[string]any {
	encoded, _ := json.Marshal(item)
	var clone map[string]any
	_ = json.Unmarshal(encoded, &clone)
	return clone
}
