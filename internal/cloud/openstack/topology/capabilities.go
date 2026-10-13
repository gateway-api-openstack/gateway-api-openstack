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
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/apiversions"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/providers"
)

const amphoraProvider = "amphora"

func (r *Resolver) checkCapabilities(ctx context.Context, request cloud.TopologyRequest) error {
	// L7 is part of Octavia v2. Tags needed for ownership require v2.5,
	// flavors v2.6, and availability zones v2.14.
	requiredMinor := 5
	if request.FlavorID != "" {
		requiredMinor = 6
	}
	if request.AvailabilityZone != "" {
		requiredMinor = 14
	}
	configuredMinor, valid := versionMinor(r.clients.LoadBalancer.Microversion)
	if !valid || configuredMinor < requiredMinor {
		return invalid("configured Octavia API version does not support the requested topology")
	}
	pages, err := apiversions.List(r.clients.LoadBalancer).AllPages(ctx)
	if err != nil {
		return readError("discover Octavia API versions", err)
	}
	versions, err := apiversions.ExtractAPIVersions(pages)
	if err != nil {
		return fmt.Errorf("extract Octavia API versions: %w", err)
	}
	supported := false
	for _, version := range versions {
		minor, valid := versionMinor(strings.TrimPrefix(version.ID, "v"))
		if valid && minor >= configuredMinor && (version.Status == "CURRENT" || version.Status == "SUPPORTED") {
			supported = true
			break
		}
	}
	if !supported {
		return invalid("Octavia does not advertise the configured API version")
	}
	providerPages, err := providers.List(r.clients.LoadBalancer, providers.ListOpts{}).AllPages(ctx)
	if err != nil {
		return readError("list Octavia providers", err)
	}
	available, err := providers.ExtractProviders(providerPages)
	if err != nil {
		return fmt.Errorf("extract Octavia providers: %w", err)
	}
	count := 0
	for _, provider := range available {
		if provider.Name == amphoraProvider {
			count++
		}
	}
	if count != 1 {
		return invalid("Octavia must advertise exactly one amphora provider")
	}
	if request.FlavorID != "" {
		if err := r.checkFlavor(ctx, request.FlavorID); err != nil {
			return err
		}
	}
	if request.AvailabilityZone != "" {
		if err := r.checkAvailabilityZone(ctx, request.AvailabilityZone); err != nil {
			return err
		}
	}
	return nil
}

func versionMinor(version string) (int, bool) {
	major, minor, found := strings.Cut(version, ".")
	if !found || major != "2" {
		return 0, false
	}
	number, err := strconv.Atoi(minor)
	return number, err == nil && number >= 0
}

func (r *Resolver) checkFlavor(ctx context.Context, id string) error {
	flavor, err := flavors.Get(ctx, r.clients.LoadBalancer, id).Extract()
	if err != nil {
		return readError("get Octavia flavor", err)
	}
	if flavor == nil || flavor.ID != id || !flavor.Enabled || flavor.FlavorProfileId == "" {
		return invalid("Octavia flavor is missing, inconsistent, or disabled")
	}
	// The provider is carried by the profile, not the public flavor. A cloud
	// denying this optional read cannot prove Amphora compatibility here.
	var response struct {
		Profile struct {
			ID       string `json:"id"`
			Provider string `json:"provider_name"`
		} `json:"flavorprofile"`
	}
	_, err = r.clients.LoadBalancer.Get(ctx, r.clients.LoadBalancer.ServiceURL("lbaas", "flavorprofiles", url.PathEscape(flavor.FlavorProfileId)), &response, nil)
	if err != nil {
		return readError("get Octavia flavor profile", err)
	}
	if response.Profile.ID != flavor.FlavorProfileId || response.Profile.Provider != amphoraProvider {
		return invalid("Octavia flavor profile does not select amphora")
	}
	return nil
}

func (r *Resolver) checkAvailabilityZone(ctx context.Context, name string) error {
	// Gophercloud has no typed Octavia availability-zone client. Keep the
	// minimal GET responses local to the OpenStack adapter.
	var response struct {
		Zone struct {
			Name      string `json:"name"`
			Enabled   bool   `json:"enabled"`
			ProfileID string `json:"availability_zone_profile_id"`
		} `json:"availability_zone"`
	}
	_, err := r.clients.LoadBalancer.Get(ctx, r.clients.LoadBalancer.ServiceURL("lbaas", "availabilityzones", url.PathEscape(name)), &response, nil)
	if err != nil {
		return readError("get Octavia availability zone", err)
	}
	if response.Zone.Name != name || !response.Zone.Enabled || response.Zone.ProfileID == "" {
		return invalid("Octavia availability zone is missing, inconsistent, or disabled")
	}
	var profileResponse struct {
		Profile struct {
			ID       string `json:"id"`
			Provider string `json:"provider_name"`
		} `json:"availability_zone_profile"`
	}
	_, err = r.clients.LoadBalancer.Get(ctx, r.clients.LoadBalancer.ServiceURL("lbaas", "availabilityzoneprofiles", url.PathEscape(response.Zone.ProfileID)), &profileResponse, nil)
	if err != nil {
		return readError("get Octavia availability zone profile", err)
	}
	if profileResponse.Profile.ID != response.Zone.ProfileID || profileResponse.Profile.Provider != amphoraProvider {
		return invalid("Octavia availability zone profile does not select amphora")
	}
	return nil
}
