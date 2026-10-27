//go:build e2e

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

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/audit"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	cloudidentity "github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/identity"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
)

func TestFloatingIPFaultOnlyMutatesExactOwnedAssociation(t *testing.T) {
	for _, operation := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(operation, func(t *testing.T) {
			testFloatingIPFaultOwnership(t, operation)
		})
	}
}

func testFloatingIPFaultOwnership(t *testing.T, operation string) {
	t.Helper()
	identity, err := cloudidentity.NewIdentity(cloud.Identity{OpenStackProjectID: "project-a", ClusterID: "cluster-a", Controller: "example.test/e2e", GatewayNamespace: "e2e", GatewayName: "edge", GatewayUID: "gateway-uid"})
	if err != nil {
		t.Fatal(err)
	}
	state := cloud.GatewayState{FloatingIPID: "fip-1", FloatingIPAddress: "203.0.113.10", VIPPortID: "vip-port", VIPAddress: "192.0.2.10"}
	for _, test := range []struct {
		name   string
		change func(*floatingips.FloatingIP)
	}{
		{name: "owned association"},
		{name: "foreign project", change: func(f *floatingips.FloatingIP) { f.ProjectID = "another-project" }},
		{name: "conflicting tenant", change: func(f *floatingips.FloatingIP) { f.TenantID = "another-project" }},
		{name: "foreign identity", change: func(f *floatingips.FloatingIP) { f.Description = "another controller" }},
		{name: "replaced resource", change: func(f *floatingips.FloatingIP) { f.ID = "fip-2" }},
		{name: "different VIP", change: func(f *floatingips.FloatingIP) { f.PortID = "another-port" }},
		{name: "different fixed IP", change: func(f *floatingips.FloatingIP) { f.FixedIP = "192.0.2.20" }},
		{name: "different network", change: func(f *floatingips.FloatingIP) { f.FloatingNetworkID = "another-network" }},
		{name: "already detached", change: func(f *floatingips.FloatingIP) { f.PortID, f.FixedIP = "", "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := floatingips.FloatingIP{ID: state.FloatingIPID, FloatingIP: state.FloatingIPAddress, FixedIP: state.VIPAddress, PortID: state.VIPPortID,
				ProjectID: "project-a", FloatingNetworkID: "external-network", Description: identity.GatewayDescription(cloudidentity.RoleFloatingIP), RevisionNumber: 7}
			if test.change != nil {
				test.change(&address)
			}
			mutations := 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/extensions/"):
					_ = json.NewEncoder(w).Encode(map[string]any{"extension": map[string]string{"alias": strings.TrimPrefix(request.URL.Path, "/extensions/")}})
				case request.Method == http.MethodGet && request.URL.Path == "/floatingips/fip-1":
					_ = json.NewEncoder(w).Encode(map[string]any{"floatingip": address})
				case request.Method == operation && request.URL.Path == "/floatingips/fip-1":
					mutations++
					if request.Header.Get("If-Match") != "revision_number=7" {
						t.Errorf("fault lacks exact revision guard: %q", request.Header.Get("If-Match"))
					}
					if operation == http.MethodDelete {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					var body map[string]map[string]any
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if request.Header.Get("If-Match") != "revision_number=7" || !reflect.DeepEqual(body, map[string]map[string]any{"floatingip": {"port_id": nil}}) {
						t.Errorf("fault mutation = %#v, revision %q", body, request.Header.Get("If-Match"))
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"floatingip": address})
				default:
					t.Errorf("unexpected request %s %s", request.Method, request.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			network := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: networkTestTransport{handler}}}, Endpoint: "http://neutron.test/"}
			fault := detachTestFloatingIP
			if operation == http.MethodDelete {
				fault = deleteTestFloatingIP
			}
			err := fault(context.Background(), network, identity, state, "external-network")
			if test.change == nil {
				if err != nil || mutations != 1 {
					t.Fatalf("owned fault = %v, mutations %d", err, mutations)
				}
			} else if err == nil || mutations != 0 {
				t.Fatalf("unsafe fault = %v, mutations %d", err, mutations)
			}
		})
	}
}

type networkTestTransport struct{ handler http.Handler }

func (transport networkTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, request)
	result := response.Result()
	result.Request = request
	return result, nil
}

func TestFloatingIPRecoveryRequiresDedicatedFaultSelection(t *testing.T) {
	shared := reportTestConfig(projectModeShared)
	shared.FloatingIPRecovery = true
	shared.Project.ExpectedExternalNetworkID = "external-network"
	for _, config := range []e2eConfig{
		{},
		{FloatingIPRecovery: true},
		shared,
	} {
		suite := &openStackSuite{config: config}
		if err := suite.verifyFloatingIPRecovery(context.Background()); err == nil {
			t.Fatal("unselected or shared project fault was accepted")
		}
	}
}

func TestFloatingIPDeletionRequiresDedicatedFaultSelection(t *testing.T) {
	shared := reportTestConfig(projectModeShared)
	shared.FloatingIPDeletion = true
	shared.Project.ExpectedExternalNetworkID = "external-network"
	for _, config := range []e2eConfig{{}, {FloatingIPDeletion: true}, shared} {
		suite := &openStackSuite{config: config}
		if err := suite.verifyFloatingIPDeletion(context.Background()); err == nil {
			t.Fatal("unselected or shared project deletion was accepted")
		}
	}
}

func TestFloatingIPDeletionStopsOnUnsupportedOrStaleRevision(t *testing.T) {
	identity, err := cloudidentity.NewIdentity(cloud.Identity{OpenStackProjectID: "project-a", ClusterID: "cluster-a", Controller: "example.test/e2e", GatewayNamespace: "e2e", GatewayName: "edge", GatewayUID: "gateway-uid"})
	if err != nil {
		t.Fatal(err)
	}
	state := cloud.GatewayState{FloatingIPID: "fip-1", FloatingIPAddress: "203.0.113.10", VIPPortID: "vip-port", VIPAddress: "192.0.2.10"}
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing extension", true: "concurrent change"}[supported], func(t *testing.T) {
			mutations := 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/extensions/"):
					if !supported {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"extension": map[string]string{"alias": strings.TrimPrefix(request.URL.Path, "/extensions/")}})
				case request.Method == http.MethodGet && request.URL.Path == "/floatingips/fip-1":
					_ = json.NewEncoder(w).Encode(map[string]any{"floatingip": floatingips.FloatingIP{ID: state.FloatingIPID, FloatingIP: state.FloatingIPAddress, FixedIP: state.VIPAddress, PortID: state.VIPPortID, ProjectID: "project-a", FloatingNetworkID: "external-network", Description: identity.GatewayDescription(cloudidentity.RoleFloatingIP), RevisionNumber: 7}})
				case request.Method == http.MethodDelete && request.URL.Path == "/floatingips/fip-1":
					mutations++
					if request.Header.Get("If-Match") != "revision_number=7" {
						t.Error("deletion lacks revision guard")
					}
					w.WriteHeader(http.StatusPreconditionFailed)
				default:
					t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})
			client := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: networkTestTransport{handler}}}, Endpoint: "http://neutron.test/"}
			err := deleteTestFloatingIP(context.Background(), client, identity, state, "external-network")
			wantMutations := 0
			if supported {
				wantMutations = 1
			}
			if err == nil || mutations != wantMutations {
				t.Fatalf("fault result = %v, mutations = %d", err, mutations)
			}
		})
	}
}

func TestFloatingIPReplacementPreservesEveryOtherResource(t *testing.T) {
	original := audit.ResourceFinding{Service: "Neutron", Type: "FloatingIP", ID: "old-address"}
	parent := audit.ResourceFinding{Service: "Octavia", Type: "LoadBalancer", ID: "lb-1"}
	before := audit.Report{Resources: []audit.ResourceFinding{original, parent}}
	for _, test := range []struct {
		name      string
		resources []audit.ResourceFinding
		rejected  bool
	}{
		{name: "only address replaced", resources: []audit.ResourceFinding{{Service: "Neutron", Type: "FloatingIP", ID: "new-address"}, parent}},
		{name: "old address retained", resources: []audit.ResourceFinding{original, parent, {Service: "Neutron", Type: "FloatingIP", ID: "new-address"}}, rejected: true},
		{name: "parent replaced", resources: []audit.ResourceFinding{{Service: "Neutron", Type: "FloatingIP", ID: "new-address"}, {Service: "Octavia", Type: "LoadBalancer", ID: "lb-2"}}, rejected: true},
		{name: "duplicate replacement", resources: []audit.ResourceFinding{{Service: "Neutron", Type: "FloatingIP", ID: "new-address"}, {Service: "Neutron", Type: "FloatingIP", ID: "new-address"}, parent}, rejected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateFloatingIPReplacementInventory(before, audit.Report{Resources: test.resources}, "old-address", "new-address")
			if (err != nil) != test.rejected {
				t.Fatalf("inventory validation = %v", err)
			}
		})
	}
}
