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
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
	"github.com/gophercloud/gophercloud/v2"
)

type fixtureResponse struct {
	status int
	body   string
}

type topologyFixture struct {
	resolver  *Resolver
	responses map[string]fixtureResponse
	serve     func(http.ResponseWriter, *http.Request) bool
	mu        sync.Mutex
	requests  []*url.URL
}

func newTopologyFixture(t *testing.T) *topologyFixture {
	t.Helper()
	f := &topologyFixture{responses: map[string]fixtureResponse{
		"/":                                                 {body: `{"versions":[{"id":"v2.14","status":"CURRENT"}]}`},
		"/v2.0/lbaas/providers":                             {body: `{"providers":[{"name":"amphora"}]}`},
		"/v2.0/subnets/vip-subnet":                          {body: `{"subnet":{"id":"vip-subnet","network_id":"private-net","project_id":"project-a","cidr":"192.0.2.0/24","ip_version":4}}`},
		"/v2.0/subnets/member-subnet":                       {body: `{"subnet":{"id":"member-subnet","network_id":"private-net","project_id":"project-a","cidr":"198.51.100.0/24","ip_version":4}}`},
		"/v2.0/networks/private-net":                        {body: `{"network":{"id":"private-net","project_id":"project-a","admin_state_up":true,"subnets":["vip-subnet","member-subnet"]}}`},
		"/v2.0/networks/public-net":                         {body: `{"network":{"id":"public-net","project_id":"operator-project","admin_state_up":true,"shared":true,"router:external":true}}`},
		"/v2.0/lbaas/flavors/flavor-1":                      {body: `{"flavor":{"id":"flavor-1","enabled":true,"flavor_profile_id":"profile-1"}}`},
		"/v2.0/lbaas/flavorprofiles/profile-1":              {body: `{"flavorprofile":{"id":"profile-1","provider_name":"amphora"}}`},
		"/v2.0/lbaas/availabilityzones/zone-1":              {body: `{"availability_zone":{"name":"zone-1","enabled":true,"availability_zone_profile_id":"az-profile-1"}}`},
		"/v2.0/lbaas/availabilityzoneprofiles/az-profile-1": {body: `{"availability_zone_profile":{"id":"az-profile-1","provider_name":"amphora"}}`},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, request.URL)
		f.mu.Unlock()
		if request.Method != http.MethodGet {
			t.Errorf("topology resolver used mutating method %s", request.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if f.serve != nil && f.serve(w, request) {
			return
		}
		response, ok := f.responses[request.URL.Path]
		if !ok {
			t.Errorf("unexpected topology request %s", request.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if response.status != 0 {
			w.WriteHeader(response.status)
		}
		_, _ = w.Write([]byte(response.body))
	}))
	t.Cleanup(server.Close)
	provider := &gophercloud.ProviderClient{HTTPClient: *server.Client()}
	f.resolver = NewResolver(clients.ServiceClients{
		ProjectID: "project-a",
		Network:   &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: server.URL + "/v2.0/", Type: "network"},
		LoadBalancer: &gophercloud.ServiceClient{
			ProviderClient: provider, Endpoint: server.URL + "/v2.0/", Type: "load-balancer", Microversion: "2.14",
		},
	}, time.Second)
	return f
}

func (f *topologyFixture) calls(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.requests {
		if request.Path == path {
			count++
		}
	}
	return count
}

func topologyRequest() cloud.TopologyRequest {
	return cloud.TopologyRequest{
		VIPSubnet: cloud.ResourceReference{ID: "vip-subnet"}, MemberSubnet: cloud.ResourceReference{ID: "member-subnet"},
	}
}

func TestResolveTopologyUsesVisibleSharedExternalNetwork(t *testing.T) {
	f := newTopologyFixture(t)
	request := topologyRequest()
	request.ExternalNetwork = &cloud.ResourceReference{ID: "public-net"}
	request.FlavorID, request.AvailabilityZone = "flavor-1", "zone-1"
	got, err := f.resolver.ResolveTopology(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := cloud.Topology{
		VIPSubnetID: "vip-subnet", MemberSubnetID: "member-subnet", ExternalNetworkID: "public-net",
		FlavorID: "flavor-1", AvailabilityZone: "zone-1",
	}
	if got != want {
		t.Fatalf("topology = %#v, want %#v", got, want)
	}
	if f.calls("/v2.0/networks/private-net") != 1 {
		t.Fatal("parent network read was not reused within the snapshot")
	}
	if _, err := f.resolver.ResolveTopology(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if f.calls("/v2.0/networks/private-net") != 2 || f.calls("/v2.0/subnets/vip-subnet") != 2 {
		t.Fatal("later evaluation reused stale infrastructure reads")
	}
}

func TestResolveTopologyReusesIdenticalSubnetReference(t *testing.T) {
	f := newTopologyFixture(t)
	request := topologyRequest()
	request.MemberSubnet = request.VIPSubnet
	got, err := f.resolver.ResolveTopology(context.Background(), request)
	if err != nil || got.VIPSubnetID != got.MemberSubnetID {
		t.Fatalf("topology = %#v, error = %v", got, err)
	}
	if f.calls("/v2.0/subnets/vip-subnet") != 1 || f.calls("/v2.0/subnets/member-subnet") != 0 {
		t.Fatal("identical subnet reference was resolved more than once")
	}
}

func TestResolveTopologySelectorPagination(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplicate=%t", duplicate), func(t *testing.T) {
			f := newTopologyFixture(t)
			request := topologyRequest()
			request.VIPSubnet = cloud.ResourceReference{Name: "vip"}
			request.MemberSubnet = cloud.ResourceReference{Tags: []string{"worker", "app"}}
			request.ExternalNetwork = &cloud.ResourceReference{Tags: []string{"public"}}
			f.serve = func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/v2.0/subnets":
					if r.URL.Query().Get("project_id") != "project-a" {
						t.Errorf("subnet selector lacks project filter: %s", r.URL)
					}
					if r.URL.Query().Get("name") == "vip" {
						_, _ = w.Write([]byte(`{"subnets":[{"id":"vip-subnet","name":"vip","network_id":"private-net","project_id":"project-a","cidr":"192.0.2.0/24","ip_version":4}]}`))
						return true
					}
					if r.URL.Query().Get("tags") != "app,worker" {
						t.Errorf("tags are not an exact deterministic AND filter: %s", r.URL)
					}
					if r.URL.Query().Get("marker") == "next" {
						body := `{"subnets":[]}`
						if duplicate {
							body = `{"subnets":[{"id":"another-member","name":"duplicate","network_id":"private-net","project_id":"project-a","tags":["app","worker"],"cidr":"203.0.113.0/24","ip_version":4}]}`
						}
						_, _ = w.Write([]byte(body))
						return true
					}
					next := f.resolver.clients.Network.ServiceURL("subnets") + "?project_id=project-a&tags=app,worker&marker=next"
					_, _ = fmt.Fprintf(w, `{"subnets":[{"id":"member-subnet","network_id":"private-net","project_id":"project-a","tags":["worker","app"],"cidr":"198.51.100.0/24","ip_version":4}],"subnets_links":[{"rel":"next","href":%q}]}`, next)
					return true
				case "/v2.0/networks":
					if r.URL.Query().Get("router:external") != "true" || r.URL.Query().Get("tags") != "public" || r.URL.Query().Has("project_id") {
						t.Errorf("unexpected external network filters: %s", r.URL)
					}
					_, _ = w.Write([]byte(`{"networks":[{"id":"public-net","project_id":"operator-project","admin_state_up":true,"router:external":true,"tags":["public"]}]}`))
					return true
				}
				return false
			}
			got, err := f.resolver.ResolveTopology(context.Background(), request)
			if duplicate {
				if !errors.Is(err, cloud.ErrTerminalValidation) || got != (cloud.Topology{}) {
					t.Fatalf("topology = %#v, error = %v, want rejection without partial topology", got, err)
				}
			} else if err != nil || got.MemberSubnetID != "member-subnet" || got.ExternalNetworkID != "public-net" {
				t.Fatalf("topology = %#v, error = %v", got, err)
			}
			if f.calls("/v2.0/subnets") != 3 {
				t.Fatal("subnet resolver did not read all selector pages")
			}
			if !slices.Equal(request.MemberSubnet.Tags, []string{"worker", "app"}) {
				t.Fatal("resolver modified the caller's selector")
			}
		})
	}
}

func TestResolveTopologyRejectsInvalidInfrastructure(t *testing.T) {
	tests := []struct{ name, path, body string }{
		{"foreign subnet", "/v2.0/subnets/vip-subnet", `{"subnet":{"id":"vip-subnet","network_id":"private-net","project_id":"foreign","cidr":"192.0.2.0/24","ip_version":4}}`},
		{"conflicting tenant", "/v2.0/subnets/vip-subnet", `{"subnet":{"id":"vip-subnet","network_id":"private-net","project_id":"project-a","tenant_id":"foreign","cidr":"192.0.2.0/24","ip_version":4}}`},
		{"unknown subnet project", "/v2.0/subnets/vip-subnet", `{"subnet":{"id":"vip-subnet","network_id":"private-net","cidr":"192.0.2.0/24","ip_version":4}}`},
		{"mismatched subnet ID", "/v2.0/subnets/vip-subnet", `{"subnet":{"id":"another-subnet","network_id":"private-net","project_id":"project-a","cidr":"192.0.2.0/24","ip_version":4}}`},
		{"CIDR family mismatch", "/v2.0/subnets/vip-subnet", `{"subnet":{"id":"vip-subnet","network_id":"private-net","project_id":"project-a","cidr":"2001:db8::/64","ip_version":4}}`},
		{"invalid CIDR", "/v2.0/subnets/vip-subnet", `{"subnet":{"id":"vip-subnet","network_id":"private-net","project_id":"project-a","cidr":"invalid","ip_version":4}}`},
		{"member family mismatch", "/v2.0/subnets/member-subnet", `{"subnet":{"id":"member-subnet","network_id":"private-net","project_id":"project-a","cidr":"2001:db8::/64","ip_version":6}}`},
		{"network missing subnet", "/v2.0/networks/private-net", `{"network":{"id":"private-net","admin_state_up":true,"subnets":["vip-subnet"]}}`},
		{"network ID mismatch", "/v2.0/networks/private-net", `{"network":{"id":"other-net","admin_state_up":true,"subnets":["vip-subnet","member-subnet"]}}`},
		{"disabled parent network", "/v2.0/networks/private-net", `{"network":{"id":"private-net","admin_state_up":false,"subnets":["vip-subnet","member-subnet"]}}`},
		{"not an external network", "/v2.0/networks/public-net", `{"network":{"id":"public-net","admin_state_up":true,"router:external":false}}`},
		{"disabled external network", "/v2.0/networks/public-net", `{"network":{"id":"public-net","admin_state_up":false,"router:external":true}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTopologyFixture(t)
			f.responses[tt.path] = fixtureResponse{body: tt.body}
			request := topologyRequest()
			request.ExternalNetwork = &cloud.ResourceReference{ID: "public-net"}
			got, err := f.resolver.ResolveTopology(context.Background(), request)
			if !errors.Is(err, cloud.ErrTerminalValidation) || got != (cloud.Topology{}) {
				t.Fatalf("topology = %#v, error = %v, want terminal validation", got, err)
			}
		})
	}
}

func TestResolveTopologyClassifiesReadFailures(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, cloud.ErrAuthentication},
		{http.StatusForbidden, cloud.ErrAuthorization},
		{http.StatusNotFound, cloud.ErrTerminalValidation},
		{http.StatusTooManyRequests, cloud.ErrRateLimited},
		{http.StatusServiceUnavailable, cloud.ErrRetryableService},
	} {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			f := newTopologyFixture(t)
			f.responses["/v2.0/subnets/member-subnet"] = fixtureResponse{status: tt.status, body: `{"message":"sensitive cloud response"}`}
			got, err := f.resolver.ResolveTopology(context.Background(), topologyRequest())
			if !errors.Is(err, tt.want) || got != (cloud.Topology{}) {
				t.Fatalf("topology = %#v, error = %v, want %v", got, err, tt.want)
			}
		})
	}
}

func TestResolveTopologyBoundsReads(t *testing.T) {
	f := newTopologyFixture(t)
	f.resolver.operationTimeout = 20 * time.Millisecond
	f.serve = func(_ http.ResponseWriter, request *http.Request) bool {
		<-request.Context().Done()
		return true
	}
	got, err := f.resolver.ResolveTopology(context.Background(), topologyRequest())
	if !errors.Is(err, cloud.ErrTimeout) || got != (cloud.Topology{}) {
		t.Fatalf("topology = %#v, error = %v, want timeout", got, err)
	}
}
