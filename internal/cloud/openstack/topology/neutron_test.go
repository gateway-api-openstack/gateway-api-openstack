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

package topology

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func TestResolveTopologyValidatesReferencesBeforeReads(t *testing.T) {
	for _, tt := range []struct {
		name      string
		reference cloud.ResourceReference
	}{
		{name: "empty"},
		{name: "ID and name", reference: cloud.ResourceReference{ID: "vip-subnet", Name: "vip"}},
		{name: "name and tags", reference: cloud.ResourceReference{Name: "vip", Tags: []string{"vip"}}},
		{name: "comma in tag", reference: cloud.ResourceReference{Tags: []string{"one,two"}}},
		{name: "duplicate tags", reference: cloud.ResourceReference{Tags: []string{"one", "one"}}},
		{name: "empty tag", reference: cloud.ResourceReference{Tags: []string{""}}},
		{name: "path in ID", reference: cloud.ResourceReference{ID: "../networks"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newTopologyFixture(t)
			request := topologyRequest()
			request.VIPSubnet = tt.reference
			got, err := f.resolver.ResolveTopology(context.Background(), request)
			if !errors.Is(err, cloud.ErrTerminalValidation) || got != (cloud.Topology{}) {
				t.Fatalf("topology = %#v, error = %v, want terminal validation", got, err)
			}
			if len(f.requests) != 0 {
				t.Fatal("invalid reference issued an API request")
			}
		})
	}
}

func TestResolveTopologyRejectsUnresolvedReferences(t *testing.T) {
	for _, tt := range []struct {
		name      string
		external  bool
		reference cloud.ResourceReference
		body      string
	}{
		{name: "missing subnet", reference: cloud.ResourceReference{Name: "vip"}, body: `{"subnets":[]}`},
		{name: "ambiguous subnet", reference: cloud.ResourceReference{Name: "vip"}, body: `{"subnets":[{"id":"one"},{"id":"two"}]}`},
		{name: "subnet name ignored by server", reference: cloud.ResourceReference{Name: "vip"}, body: `{"subnets":[{"id":"vip-subnet","name":"other","project_id":"project-a","network_id":"private-net","cidr":"192.0.2.0/24","ip_version":4}]}`},
		{name: "subnet tag filter ignored", reference: cloud.ResourceReference{Tags: []string{"one", "two"}}, body: `{"subnets":[{"id":"vip-subnet","tags":["one"],"project_id":"project-a","network_id":"private-net","cidr":"192.0.2.0/24","ip_version":4}]}`},
		{name: "missing external network", external: true, reference: cloud.ResourceReference{Name: "public"}, body: `{"networks":[]}`},
		{name: "ambiguous external network", external: true, reference: cloud.ResourceReference{Name: "public"}, body: `{"networks":[{"id":"one"},{"id":"two"}]}`},
		{name: "external network name ignored", external: true, reference: cloud.ResourceReference{Name: "public"}, body: `{"networks":[{"id":"public-net","name":"other","admin_state_up":true,"router:external":true}]}`},
		{name: "external tag filter ignored", external: true, reference: cloud.ResourceReference{Tags: []string{"one", "two"}}, body: `{"networks":[{"id":"public-net","tags":["one"],"admin_state_up":true,"router:external":true}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newTopologyFixture(t)
			request := topologyRequest()
			path := "/v2.0/subnets"
			if tt.external {
				request.ExternalNetwork = &tt.reference
				path = "/v2.0/networks"
			} else {
				request.VIPSubnet = tt.reference
			}
			f.responses[path] = fixtureResponse{body: tt.body}
			got, err := f.resolver.ResolveTopology(context.Background(), request)
			if !errors.Is(err, cloud.ErrTerminalValidation) || got != (cloud.Topology{}) {
				t.Fatalf("topology = %#v, error = %v, want terminal validation", got, err)
			}
		})
	}
}

func TestResolveTopologyFindsDuplicateExternalNetworkOnLaterPage(t *testing.T) {
	f := newTopologyFixture(t)
	request := topologyRequest()
	request.ExternalNetwork = &cloud.ResourceReference{Name: "public"}
	f.serve = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v2.0/networks" {
			return false
		}
		if r.URL.Query().Get("name") != "public" || r.URL.Query().Get("router:external") != "true" {
			t.Errorf("missing external network filters: %s", r.URL)
		}
		if r.URL.Query().Get("marker") == "next" {
			_, _ = w.Write([]byte(`{"networks":[{"id":"second-network","name":"public","admin_state_up":true,"router:external":true}]}`))
			return true
		}
		next := f.resolver.clients.Network.ServiceURL("networks") + "?name=public&router:external=true&marker=next"
		_, _ = fmt.Fprintf(w, `{"networks":[{"id":"public-net","name":"public","admin_state_up":true,"router:external":true}],"networks_links":[{"rel":"next","href":%q}]}`, next)
		return true
	}
	got, err := f.resolver.ResolveTopology(context.Background(), request)
	if !errors.Is(err, cloud.ErrTerminalValidation) || got != (cloud.Topology{}) || f.calls("/v2.0/networks") != 2 {
		t.Fatalf("topology = %#v, error = %v, want duplicate rejection after pagination", got, err)
	}
}

func TestResolveTopologyIPv6(t *testing.T) {
	f := newTopologyFixture(t)
	f.responses["/v2.0/subnets/vip-subnet"] = fixtureResponse{body: `{"subnet":{"id":"vip-subnet","network_id":"private-net","project_id":"project-a","cidr":"2001:db8::/64","ip_version":6}}`}
	f.responses["/v2.0/subnets/member-subnet"] = fixtureResponse{body: `{"subnet":{"id":"member-subnet","network_id":"private-net","project_id":"project-a","cidr":"2001:db8:1::/64","ip_version":6}}`}
	request := topologyRequest()
	if _, err := f.resolver.ResolveTopology(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.ExternalNetwork = &cloud.ResourceReference{ID: "public-net"}
	got, err := f.resolver.ResolveTopology(context.Background(), request)
	if !errors.Is(err, cloud.ErrTerminalValidation) || got != (cloud.Topology{}) {
		t.Fatalf("topology = %#v, error = %v, want rejection of IPv6 Floating IP placement", got, err)
	}
}
