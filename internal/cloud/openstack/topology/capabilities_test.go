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
	"net/http"
	"reflect"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func TestResolveTopologyRejectsUnknownCapabilities(t *testing.T) {
	tests := []struct{ name, path, body string }{
		{"tags unavailable", "/", `{"versions":[{"id":"v2.4","status":"CURRENT"}]}`},
		{"only unknown major", "/", `{"versions":[{"id":"v3.0","status":"CURRENT"}]}`},
		{"unknown version status", "/", `{"versions":[{"id":"v2.14","status":"EXPERIMENTAL"}]}`},
		{"no version", "/", `{"versions":[]}`},
		{"no amphora provider", "/v2.0/lbaas/providers", `{"providers":[{"name":"ovn"}]}`},
		{"provider alias", "/v2.0/lbaas/providers", `{"providers":[{"name":"octavia"}]}`},
		{"duplicate provider", "/v2.0/lbaas/providers", `{"providers":[{"name":"amphora"},{"name":"amphora"}]}`},
		{"disabled flavor", "/v2.0/lbaas/flavors/flavor-1", `{"flavor":{"id":"flavor-1","enabled":false,"flavor_profile_id":"profile-1"}}`},
		{"inconsistent flavor", "/v2.0/lbaas/flavors/flavor-1", `{"flavor":{"id":"another-flavor","enabled":true,"flavor_profile_id":"profile-1"}}`},
		{"unknown flavor profile", "/v2.0/lbaas/flavors/flavor-1", `{"flavor":{"id":"flavor-1","enabled":true}}`},
		{"foreign flavor provider", "/v2.0/lbaas/flavorprofiles/profile-1", `{"flavorprofile":{"id":"profile-1","provider_name":"ovn"}}`},
		{"disabled zone", "/v2.0/lbaas/availabilityzones/zone-1", `{"availability_zone":{"name":"zone-1","enabled":false,"availability_zone_profile_id":"az-profile-1"}}`},
		{"inconsistent zone", "/v2.0/lbaas/availabilityzones/zone-1", `{"availability_zone":{"name":"other-zone","enabled":true,"availability_zone_profile_id":"az-profile-1"}}`},
		{"unknown zone profile", "/v2.0/lbaas/availabilityzones/zone-1", `{"availability_zone":{"name":"zone-1","enabled":true}}`},
		{"foreign zone provider", "/v2.0/lbaas/availabilityzoneprofiles/az-profile-1", `{"availability_zone_profile":{"id":"az-profile-1","provider_name":"ovn"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTopologyFixture(t)
			f.responses[tt.path] = fixtureResponse{body: tt.body}
			request := topologyRequest()
			request.FlavorID, request.AvailabilityZone = "flavor-1", "zone-1"
			got, err := f.resolver.ResolveTopology(context.Background(), request)
			if !errors.Is(err, cloud.ErrTerminalValidation) || !reflect.DeepEqual(got, cloud.Topology{}) {
				t.Fatalf("topology = %#v, error = %v, want terminal validation", got, err)
			}
			if f.calls("/v2.0/subnets/vip-subnet") != 0 {
				t.Fatal("unsupported capability did not stop resolution")
			}
		})
	}
}

func TestResolveTopologyChecksConfiguredVersion(t *testing.T) {
	for _, tt := range []struct {
		name, version, flavor, zone string
		valid                       bool
	}{
		{name: "tag baseline", version: "2.5", valid: true},
		{name: "old client", version: "2.4"},
		{name: "invalid client version", version: "unknown"},
		{name: "newer than endpoint", version: "2.15"},
		{name: "flavor needs 2.6", version: "2.5", flavor: "flavor-1"},
		{name: "flavor supported", version: "2.6", flavor: "flavor-1", valid: true},
		{name: "zone needs 2.14", version: "2.13", zone: "zone-1"},
		{name: "zone supported", version: "2.14", zone: "zone-1", valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newTopologyFixture(t)
			f.resolver.clients.LoadBalancer.Microversion = tt.version
			request := topologyRequest()
			request.FlavorID, request.AvailabilityZone = tt.flavor, tt.zone
			got, err := f.resolver.ResolveTopology(context.Background(), request)
			if tt.valid {
				if err != nil || got.FlavorID != tt.flavor || got.AvailabilityZone != tt.zone {
					t.Fatalf("topology = %#v, error = %v", got, err)
				}
			} else if !errors.Is(err, cloud.ErrTerminalValidation) || !reflect.DeepEqual(got, cloud.Topology{}) {
				t.Fatalf("topology = %#v, error = %v, want terminal validation", got, err)
			}
		})
	}
}

func TestResolveTopologyRequiresOptionalProfileRead(t *testing.T) {
	for _, path := range []string{"/v2.0/lbaas/flavorprofiles/profile-1", "/v2.0/lbaas/availabilityzoneprofiles/az-profile-1"} {
		t.Run(path, func(t *testing.T) {
			f := newTopologyFixture(t)
			f.responses[path] = fixtureResponse{status: http.StatusForbidden, body: `{"message":"profile read requires additional permission"}`}
			request := topologyRequest()
			request.FlavorID, request.AvailabilityZone = "flavor-1", "zone-1"
			got, err := f.resolver.ResolveTopology(context.Background(), request)
			if !errors.Is(err, cloud.ErrAuthorization) || !reflect.DeepEqual(got, cloud.Topology{}) {
				t.Fatalf("topology = %#v, error = %v, want authorization failure", got, err)
			}
		})
	}
}
