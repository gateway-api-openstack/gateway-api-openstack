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
	"net/http"
	"slices"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

const testServerID = "aa151e7a-1ac7-4344-9f33-b5ad9b9fdb4a"

func validMemberPort() map[string]any {
	return map[string]any{
		"id": "worker-port", "project_id": "project-a", "tenant_id": "project-a",
		"device_id": testServerID, "device_owner": "compute:nova", "admin_state_up": true,
		"port_security_enabled": true, "security_groups": []string{"foreign-group", "sg-a"},
		"fixed_ips": []map[string]string{{"ip_address": "198.51.100.7", "subnet_id": "member-subnet"}},
	}
}

func TestMemberPortValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(map[string]any)
		allowed bool
	}{
		{name: "valid port with foreign associations", allowed: true},
		{name: "foreign project", mutate: func(port map[string]any) { port["project_id"] = "foreign" }},
		{name: "inconsistent tenant", mutate: func(port map[string]any) { port["tenant_id"] = "foreign" }},
		{name: "different server", mutate: func(port map[string]any) { port["device_id"] = "other-server" }},
		{name: "noncompute owner", mutate: func(port map[string]any) { port["device_owner"] = "network:router_interface" }},
		{name: "disabled port", mutate: func(port map[string]any) { port["admin_state_up"] = false }},
		{name: "disabled security", mutate: func(port map[string]any) { port["port_security_enabled"] = false }},
		{name: "unknown security", mutate: func(port map[string]any) { delete(port, "port_security_enabled") }},
		{name: "missing group", mutate: func(port map[string]any) { port["security_groups"] = []string{"foreign-group"} }},
		{name: "different fixed IP", mutate: func(port map[string]any) {
			port["fixed_ips"] = []map[string]string{{"ip_address": "198.51.100.8", "subnet_id": "member-subnet"}}
		}},
		{name: "different subnet", mutate: func(port map[string]any) {
			port["fixed_ips"] = []map[string]string{{"ip_address": "198.51.100.7", "subnet_id": "another-subnet"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newTopologyFixture(t)
			port := validMemberPort()
			if test.mutate != nil {
				test.mutate(port)
			}
			f.serve = func(w http.ResponseWriter, request *http.Request) bool {
				if request.URL.Path != "/v2.0/ports" {
					return false
				}
				query := request.URL.Query()
				if query.Get("project_id") != "project-a" || query.Get("device_id") != testServerID ||
					!slices.Contains(query["fixed_ips"], "ip_address=198.51.100.7") || !slices.Contains(query["fixed_ips"], "subnet_id=member-subnet") {
					t.Errorf("port mapping did not narrow the query: %s", request.URL)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"ports": []any{port}}); err != nil {
					t.Error(err)
				}
				return true
			}
			err := f.resolver.ValidateMemberPort(context.Background(), "openstack:///"+testServerID, "198.51.100.7", "member-subnet", []string{"sg-a"})
			if test.allowed && err != nil || !test.allowed && !errors.Is(err, cloud.ErrTerminalValidation) {
				t.Fatalf("port validation = %v, want allowed %t", err, test.allowed)
			}
		})
	}
}

func TestMemberPortRejectsAmbiguousPages(t *testing.T) {
	f := newTopologyFixture(t)
	f.serve = func(w http.ResponseWriter, request *http.Request) bool {
		if request.URL.Path != "/v2.0/ports" {
			return false
		}
		response := map[string]any{"ports": []any{validMemberPort()}}
		if request.URL.Query().Get("marker") == "" {
			response["ports_links"] = []map[string]string{{"rel": "next", "href": f.resolver.clients.Network.ServiceURL("ports") + "?marker=worker-port"}}
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
		return true
	}
	if err := f.resolver.ValidateMemberPort(context.Background(), "openstack:///"+testServerID, "198.51.100.7", "member-subnet", nil); !errors.Is(err, cloud.ErrTerminalValidation) {
		t.Fatalf("ambiguous port error = %v", err)
	}
	if f.calls("/v2.0/ports") != 2 {
		t.Fatal("did not read every port page")
	}
}

func TestProviderIDRequiresExactCloudIdentity(t *testing.T) {
	for _, test := range []struct {
		providerID string
		allowed    bool
	}{
		{"openstack:///" + testServerID, true},
		{"openstack://RegionOne/" + testServerID, true},
		{"openstack://another-region/" + testServerID, false},
		{"openstack:/" + testServerID, false},
		{"openstack:///" + testServerID + "?server=other", false},
		{"openstack:///worker-name", false},
		{"openstack:///" + testServerID + "/extra", false},
		{"aws:///" + testServerID, false},
		{"", false},
	} {
		t.Run(test.providerID, func(t *testing.T) {
			id, err := serverIDFromProviderID(test.providerID, "RegionOne")
			if test.allowed {
				if err != nil || id != testServerID {
					t.Fatalf("server identity = %q, %v", id, err)
				}
			} else if id != "" || !errors.Is(err, cloud.ErrTerminalValidation) {
				t.Fatalf("invalid server identity = %q, %v", id, err)
			}
		})
	}
}
