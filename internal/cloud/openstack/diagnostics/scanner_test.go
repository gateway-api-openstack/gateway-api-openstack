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

package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/diagnostics"
	"github.com/gophercloud/gophercloud/v2"
)

func TestInspectReadOnlyAndRedacted(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		if request.Method != http.MethodGet {
			t.Errorf("unexpected mutation %s", request.Method)
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/security-groups":
			if request.URL.Query().Get("project_id") != "private-project" || request.URL.Query().Get("limit") != "1" {
				t.Errorf("security group request is not scoped and bounded: %s", request.URL.RawQuery)
			}
			_, _ = fmt.Fprint(writer, `{"security_groups":[{"id":"private-group","name":"private-name"}]}`)
		case "/quotas/private-project/details.json":
			_, _ = fmt.Fprint(writer, `{"quota":{"floatingip":{"used":2,"reserved":0,"limit":3}}}`)
		case "/v1/secrets":
			if request.URL.Query().Get("limit") != "1" {
				t.Error("Barbican metadata read is not bounded")
			}
			_, _ = fmt.Fprint(writer, `{"secrets":[{"name":"private-certificate","secret_ref":"https://private-cloud/secrets/private-secret"}]}`)
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	serviceClients := testClients(server.URL)
	serviceClients.Network.EndpointLocator = func(options gophercloud.EndpointOpts) (string, error) {
		if options.Type != "key-manager" {
			t.Errorf("unexpected service type %q", options.Type)
		}
		return server.URL + "/", nil
	}
	reader := &fakeTopology{resolved: cloud.Topology{MemberSubnetID: "private-subnet", BackendSecurityGroupIDs: []string{"private-group"}}}
	scanner := &Scanner{clients: serviceClients, topology: reader, timeout: time.Minute}
	report := scanner.Inspect(context.Background(), diagnostics.Request{
		Topology: cloud.TopologyRequest{ExternalNetwork: &cloud.ResourceReference{ID: "private-network"}},
		Nodes: []diagnostics.Node{
			{ProviderID: "private-node", Address: "192.0.2.7"},
			{ProviderID: "private-node-2", Address: "2001:db8::7"},
		},
		CheckBarbican: true,
	})
	if report.HasFailures() || len(reader.nodes) != 2 || len(requests) != 3 {
		t.Fatalf("unexpected report or calls: report=%+v, nodes=%d, requests=%v", report, len(reader.nodes), requests)
	}
	for _, name := range []string{"securityGroupUpdate", "backendReachability", "dataPlaneTopology"} {
		assertStatus(t, report, name, diagnostics.Unknown)
	}
	assertStatus(t, report, "floatingIPQuota", diagnostics.Passed)
	assertStatus(t, report, "barbican", diagnostics.Passed)
	if strings.Join(report.IPFamilies, ",") != "IPv4,IPv6" {
		t.Fatalf("IP families = %v", report.IPFamilies)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-", "192.0.2.7", "2001:db8::7", server.URL} {
		if strings.Contains(string(data), private) {
			t.Errorf("report exposed %q", private)
		}
	}
}

func TestInspectFailureDoesNotAuthorizeDependentChecks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected mutation %s", request.Method)
		}
		writer.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(writer, `{"error":"private-token-and-cloud-details"}`)
	}))
	defer server.Close()
	reader := &fakeTopology{err: cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, errors.New("private-subnet"))}
	scanner := &Scanner{clients: testClients(server.URL), topology: reader, timeout: time.Minute}
	report := scanner.Inspect(context.Background(), diagnostics.Request{
		Topology: cloud.TopologyRequest{ExternalNetwork: &cloud.ResourceReference{ID: "private-network"}},
		Nodes:    []diagnostics.Node{{ProviderID: "private-node", Address: "192.0.2.7"}},
	})
	if !report.HasFailures() || len(reader.nodes) != 0 {
		t.Fatalf("failed topology authorized node validation: %+v", report)
	}
	assertStatus(t, report, "nodePort", diagnostics.Skipped)
	assertStatus(t, report, "floatingIPQuota", diagnostics.Unknown)
	assertStatus(t, report, "securityGroupRead", diagnostics.Failed)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") {
		t.Fatalf("provider error leaked: %s", encoded)
	}
}

func TestFloatingIPQuotaIncludesReservations(t *testing.T) {
	for _, test := range []struct {
		name   string
		quota  string
		status diagnostics.Status
	}{
		{"available", `{"used":2,"reserved":0,"limit":3}`, diagnostics.Passed},
		{"reserved", `{"used":2,"reserved":1,"limit":3}`, diagnostics.Failed},
		{"unlimited", `{"used":100,"reserved":1,"limit":-1}`, diagnostics.Passed},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/quotas/private-project/details.json" {
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(writer, `{"quota":{"floatingip":%s}}`, test.quota)
			}))
			defer server.Close()
			scanner := &Scanner{clients: testClients(server.URL)}
			report := diagnostics.NewReport()
			scanner.inspectFloatingIPQuota(context.Background(), &report)
			assertStatus(t, report, "floatingIPQuota", test.status)
		})
	}
}

func TestInspectPreservesMemberFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"security_groups":[]}`)
	}))
	defer server.Close()
	reader := &fakeTopology{memberErr: errors.New("private-port")}
	scanner := &Scanner{clients: testClients(server.URL), topology: reader, timeout: time.Minute}
	report := scanner.Inspect(context.Background(), diagnostics.Request{Nodes: []diagnostics.Node{{ProviderID: "node"}}})
	assertStatus(t, report, "nodePort", diagnostics.Failed)
	assertStatus(t, report, "floatingIPQuota", diagnostics.Skipped)
	assertStatus(t, report, "barbican", diagnostics.Skipped)
}

func TestNodeDiagnosticsAllowUnmanagedPortSecurity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/ports" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"ports":[{"id":"port","project_id":"private-project","device_id":"12345678-1234-1234-1234-123456789abc","device_owner":"compute:nova","admin_state_up":true,"port_security_enabled":false,"fixed_ips":[{"subnet_id":"subnet","ip_address":"192.0.2.7"}],"security_groups":["group"]}]}`)
	}))
	defer server.Close()
	for _, test := range []struct {
		name   string
		groups []string
		status diagnostics.Status
	}{
		{name: "unmanaged", status: diagnostics.Passed},
		{name: "referenced", groups: []string{"group"}, status: diagnostics.Failed},
	} {
		t.Run(test.name, func(t *testing.T) {
			scanner := NewScanner(testClients(server.URL), time.Minute)
			report := diagnostics.NewReport()
			scanner.inspectNodes(context.Background(), []diagnostics.Node{{
				ProviderID: "openstack:///12345678-1234-1234-1234-123456789abc", Address: "192.0.2.7",
			}}, cloud.Topology{MemberSubnetID: "subnet", BackendSecurityGroupIDs: test.groups}, true, &report)
			assertStatus(t, report, "nodePort", test.status)
		})
	}
}

type fakeTopology struct {
	resolved  cloud.Topology
	err       error
	memberErr error
	nodes     []diagnostics.Node
}

func (f *fakeTopology) ResolveTopology(context.Context, cloud.TopologyRequest) (cloud.Topology, error) {
	return f.resolved, f.err
}

func (f *fakeTopology) ValidateMemberPort(_ context.Context, providerID, address, _ string, _ []string) error {
	f.nodes = append(f.nodes, diagnostics.Node{ProviderID: providerID, Address: address})
	return f.memberErr
}

func testClients(endpoint string) clients.ServiceClients {
	provider := &gophercloud.ProviderClient{}
	service := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: endpoint + "/"}
	return clients.ServiceClients{Network: service, LoadBalancer: service, ProjectID: "private-project"}
}

func assertStatus(t *testing.T, report diagnostics.Report, name string, status diagnostics.Status) {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			if check.Status != status {
				t.Errorf("check %s = %s, want %s", name, check.Status, status)
			}
			return
		}
	}
	t.Fatalf("missing check %s", name)
}
