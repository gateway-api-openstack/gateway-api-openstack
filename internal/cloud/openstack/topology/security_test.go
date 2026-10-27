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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
)

func TestResolveSecurityGroupPaginationAndProjectScope(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%t", ambiguous), func(t *testing.T) {
			f := newTopologyFixture(t)
			f.serve = func(w http.ResponseWriter, request *http.Request) bool {
				if request.URL.Path != "/v2.0/security-groups" {
					return false
				}
				query := request.URL.Query()
				if query.Get("project_id") != "project-a" || query.Get("tags") != "backend,test" {
					t.Errorf("unscoped selector: %s", request.URL)
				}
				if query.Get("marker") == "" {
					next := f.resolver.clients.Network.ServiceURL("security-groups") + "?project_id=project-a&tags=backend,test&marker=sg-a"
					_, _ = fmt.Fprintf(w, `{"security_groups":[{"id":"sg-a","project_id":"project-a","tags":["backend","test"]}],"security_groups_links":[{"rel":"next","href":%q}]}`, next)
				} else if ambiguous {
					_, _ = fmt.Fprint(w, `{"security_groups":[{"id":"sg-b","project_id":"project-a","tags":["backend","test"]}]}`)
				} else {
					_, _ = fmt.Fprint(w, `{"security_groups":[]}`)
				}
				return true
			}
			ids, err := f.resolver.resolveSecurityGroups(context.Background(), []cloud.ResourceReference{{Tags: []string{"test", "backend"}}})
			if ambiguous {
				if !errors.Is(err, cloud.ErrTerminalValidation) || ids != nil {
					t.Fatalf("ambiguous groups = %v, %v", ids, err)
				}
			} else if err != nil || !reflect.DeepEqual(ids, []string{"sg-a"}) {
				t.Fatalf("resolved groups = %v, %v", ids, err)
			}
			if f.calls("/v2.0/security-groups") != 2 {
				t.Fatal("did not read every selector page")
			}
		})
	}
}

func TestResolveSecurityGroupsRejectsForeignAndDuplicateReferences(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		refs []cloud.ResourceReference
	}{
		{"foreign project", `{"id":"sg-a","project_id":"foreign"}`, []cloud.ResourceReference{{ID: "sg-a"}}},
		{"inconsistent tenant", `{"id":"sg-a","project_id":"project-a","tenant_id":"foreign"}`, []cloud.ResourceReference{{ID: "sg-a"}}},
		{"wrong ID", `{"id":"sg-b","project_id":"project-a"}`, []cloud.ResourceReference{{ID: "sg-a"}}},
		{"duplicate resolution", `{"id":"sg-a","name":"backend","project_id":"project-a"}`, []cloud.ResourceReference{{ID: "sg-a"}, {Name: "backend"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newTopologyFixture(t)
			f.responses["/v2.0/security-groups/sg-a"] = fixtureResponse{body: `{"security_group":` + test.body + `}`}
			f.responses["/v2.0/security-groups"] = fixtureResponse{body: `{"security_groups":[` + test.body + `]}`}
			if ids, err := f.resolver.resolveSecurityGroups(context.Background(), test.refs); ids != nil || !errors.Is(err, cloud.ErrTerminalValidation) {
				t.Fatalf("resolved groups = %v, %v", ids, err)
			}
		})
	}
}

func TestCustomVIPSecurityRequiresAPIAndFlavorSupport(t *testing.T) {
	for _, test := range []struct {
		name, configured, advertised, flavorData string
		withFlavor, allowed                      bool
	}{
		{name: "supported", configured: "2.29", advertised: "v2.29", allowed: true},
		{name: "configured too old", configured: "2.28", advertised: "v2.29"},
		{name: "advertised too old", configured: "2.29", advertised: "v2.28"},
		{name: "non SR-IOV flavor", configured: "2.29", advertised: "v2.29", withFlavor: true, flavorData: `{}`, allowed: true},
		{name: "SR-IOV flavor", configured: "2.29", advertised: "v2.29", withFlavor: true, flavorData: `{"sriov_vip":true}`},
		{name: "unknown flavor data", configured: "2.29", advertised: "v2.29", withFlavor: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newTopologyFixture(t)
			f.resolver.clients.LoadBalancer.Microversion = test.configured
			f.responses["/"] = fixtureResponse{body: fmt.Sprintf(`{"versions":[{"id":%q,"status":"CURRENT"}]}`, test.advertised)}
			f.responses["/v2.0/security-groups/sg-a"] = fixtureResponse{body: `{"security_group":{"id":"sg-a","project_id":"project-a"}}`}
			f.responses["/v2.0/lbaas/flavorprofiles/profile-1"] = fixtureResponse{body: fmt.Sprintf(`{"flavorprofile":{"id":"profile-1","provider_name":"amphora","flavor_data":%q}}`, test.flavorData)}
			request := topologyRequest()
			request.VIPSecurityGroups = []cloud.ResourceReference{{ID: "sg-a"}}
			if test.withFlavor {
				request.FlavorID = "flavor-1"
			}
			got, err := f.resolver.ResolveTopology(context.Background(), request)
			if test.allowed {
				if err != nil || !reflect.DeepEqual(got.VIPSecurityGroupIDs, []string{"sg-a"}) {
					t.Fatalf("topology = %#v, %v", got, err)
				}
			} else if !errors.Is(err, cloud.ErrTerminalValidation) || !reflect.DeepEqual(got, cloud.Topology{}) {
				t.Fatalf("unsupported topology = %#v, %v", got, err)
			}
		})
	}
}

func TestSecurityRulePrerequisites(t *testing.T) {
	for _, test := range []struct {
		name    string
		rule    rules.SecGroupRule
		allowed bool
	}{
		{"exact TCP port", rules.SecGroupRule{Direction: "ingress", EtherType: "IPv4", Protocol: "tcp", PortRangeMin: 30080, PortRangeMax: 30080}, true},
		{"numeric TCP range", rules.SecGroupRule{Direction: "ingress", EtherType: "IPv4", Protocol: "6", PortRangeMin: 30000, PortRangeMax: 31000}, true},
		{"all protocols", rules.SecGroupRule{Direction: "ingress", EtherType: "IPv4"}, true},
		{"wrong family", rules.SecGroupRule{Direction: "ingress", EtherType: "IPv6"}, false},
		{"egress only", rules.SecGroupRule{Direction: "egress", EtherType: "IPv4"}, false},
		{"wrong protocol", rules.SecGroupRule{Direction: "ingress", EtherType: "IPv4", Protocol: "udp"}, false},
		{"wrong port", rules.SecGroupRule{Direction: "ingress", EtherType: "IPv4", Protocol: "tcp", PortRangeMin: 80, PortRangeMax: 80}, false},
		{"malformed source", rules.SecGroupRule{Direction: "ingress", EtherType: "IPv4", RemoteIPPrefix: "invalid"}, false},
		// This is only a prerequisite: a source selector is not proof that an
		// Amphora or the test client belongs to it.
		{"restricted source", rules.SecGroupRule{Direction: "ingress", EtherType: "IPv4", RemoteIPPrefix: "192.0.2.0/24"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := allowsTCPIngress([]groups.SecGroup{{Rules: []rules.SecGroupRule{test.rule}}}, 4, 30080); got != test.allowed {
				t.Fatalf("allowed = %t, want %t", got, test.allowed)
			}
		})
	}
}

func TestGatewaySecurityObservesExternalRepairs(t *testing.T) {
	f := newTopologyFixture(t)
	if err := f.resolver.ValidateGatewaySecurity(context.Background(), cloud.GatewaySpec{}, nil); err != nil || len(f.requests) != 0 {
		t.Fatalf("unmanaged security performed reads: %v, %v", f.requests, err)
	}
	f.resolver.clients.LoadBalancer.Microversion = "2.29"
	f.responses["/"] = fixtureResponse{body: `{"versions":[{"id":"v2.29","status":"CURRENT"}]}`}
	f.responses["/v2.0/security-groups/sg-a"] = fixtureResponse{body: `{"security_group":{"id":"sg-a","project_id":"project-a","security_group_rules":[]}}`}
	spec := cloud.GatewaySpec{VIPSubnetID: "vip-subnet", ListenerPort: 80, VIPSecurityGroupIDs: []string{"sg-a"}}
	if err := f.resolver.ValidateGatewaySecurity(context.Background(), spec, nil); !errors.Is(err, cloud.ErrDependencyNotReady) {
		t.Fatalf("missing ingress must be rechecked: %v", err)
	}
	f.responses["/v2.0/security-groups/sg-a"] = fixtureResponse{body: `{"security_group":{"id":"sg-a","project_id":"project-a","security_group_rules":[{"direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":80,"port_range_max":80}]}}`}
	if err := f.resolver.ValidateGatewaySecurity(context.Background(), spec, nil); err != nil {
		t.Fatalf("did not observe repaired rule: %v", err)
	}
	if f.calls("/v2.0/security-groups/sg-a") != 2 {
		t.Fatal("reused security rules across snapshots")
	}
}

func TestBackendSecurityChecksPortAndScopesFailure(t *testing.T) {
	f := newTopologyFixture(t)
	f.responses["/v2.0/security-groups/sg-a"] = fixtureResponse{body: `{"security_group":{"id":"sg-a","project_id":"project-a","security_group_rules":[{"direction":"ingress","ethertype":"IPv4","protocol":"tcp"}]}}`}
	port := validMemberPort()
	body, err := json.Marshal(map[string]any{"ports": []any{port}})
	if err != nil {
		t.Fatal(err)
	}
	f.responses["/v2.0/ports"] = fixtureResponse{body: string(body)}
	route := cloud.RouteSpec{Identity: cloud.Identity{RouteUID: "route-a"}, MemberSubnetID: "member-subnet", BackendSecurityGroupIDs: []string{"sg-a"}, Members: []cloud.Member{{Address: "198.51.100.7", Port: 30080, ProviderID: "openstack:///" + testServerID}}}
	if err := f.resolver.ValidateGatewaySecurity(context.Background(), cloud.GatewaySpec{}, []cloud.RouteSpec{route}); err != nil {
		t.Fatal(err)
	}
	f.responses["/v2.0/ports"] = fixtureResponse{body: `{"ports":[]}`}
	err = f.resolver.ValidateGatewaySecurity(context.Background(), cloud.GatewaySpec{}, []cloud.RouteSpec{route})
	var graphError *cloud.GraphError
	if !errors.Is(err, cloud.ErrDependencyNotReady) || !errors.As(err, &graphError) || graphError.Identity != route.Identity {
		t.Fatalf("missing backend port error = %v", err)
	}
}
