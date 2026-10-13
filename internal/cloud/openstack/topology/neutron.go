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
	"net/http"
	"net/netip"
	"slices"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
)

func (r *Resolver) resolveSubnet(ctx context.Context, reference cloud.ResourceReference) (*subnets.Subnet, error) {
	var subnet *subnets.Subnet
	if reference.ID != "" {
		var err error
		subnet, err = subnets.Get(ctx, r.clients.Network, reference.ID).Extract()
		if err != nil {
			return nil, readError("get subnet", err)
		}
	} else {
		pages, err := subnets.List(r.clients.Network, subnets.ListOpts{
			ProjectID: r.clients.ProjectID, Name: reference.Name, Tags: tagFilter(reference.Tags),
		}).AllPages(ctx)
		if err != nil {
			return nil, readError("list subnets", err)
		}
		matches, err := subnets.ExtractSubnets(pages)
		if err != nil {
			return nil, fmt.Errorf("extract subnets: %w", err)
		}
		if len(matches) != 1 {
			return nil, invalid("subnet reference must resolve to exactly one resource")
		}
		subnet = &matches[0]
	}
	if subnet == nil || subnet.ID == "" || subnet.NetworkID == "" ||
		!referenceMatches(reference, subnet.ID, subnet.Name, subnet.Tags) {
		return nil, invalid("resolved subnet does not match its reference")
	}
	if subnet.ProjectID != r.clients.ProjectID || subnet.TenantID != "" && subnet.TenantID != r.clients.ProjectID {
		return nil, invalid("subnet does not belong to the authenticated project")
	}
	prefix, err := netip.ParsePrefix(subnet.CIDR)
	if err != nil || subnet.IPVersion != 4 && subnet.IPVersion != 6 ||
		prefix.Addr().Is4() != (subnet.IPVersion == 4) || prefix.Addr().Is4In6() {
		return nil, invalid("subnet CIDR does not match its IP version")
	}
	return subnet, nil
}

type externalNetwork struct {
	networks.Network
	external.NetworkExternalExt
}

func (r *Resolver) resolveExternalNetwork(ctx context.Context, reference cloud.ResourceReference) (string, error) {
	var network externalNetwork
	if reference.ID != "" {
		if err := networks.Get(ctx, r.clients.Network, reference.ID).ExtractInto(&network); err != nil {
			return "", readError("get external network", err)
		}
	} else {
		// Public external networks commonly belong to the operator's project.
		// Scope this read to the authenticated client's visibility rather than
		// incorrectly filtering them by the requesting project's ownership.
		externalOnly := true
		pages, err := networks.List(r.clients.Network, external.ListOptsExt{
			ListOptsBuilder: networks.ListOpts{Name: reference.Name, Tags: tagFilter(reference.Tags)},
			External:        &externalOnly,
		}).AllPages(ctx)
		if err != nil {
			return "", readError("list external networks", err)
		}
		var matches []externalNetwork
		if err := networks.ExtractNetworksInto(pages, &matches); err != nil {
			return "", fmt.Errorf("extract external networks: %w", err)
		}
		if len(matches) != 1 {
			return "", invalid("external network reference must resolve to exactly one resource")
		}
		network = matches[0]
	}
	if network.ID == "" || !referenceMatches(reference, network.ID, network.Name, network.Tags) {
		return "", invalid("resolved external network does not match its reference")
	}
	if !network.External || !network.AdminStateUp {
		return "", invalid("selected network must be external and administratively enabled")
	}
	return network.ID, nil
}

func referenceMatches(reference cloud.ResourceReference, id, name string, tags []string) bool {
	if reference.ID != "" && reference.ID != id || reference.Name != "" && reference.Name != name {
		return false
	}
	for _, tag := range reference.Tags {
		if !slices.Contains(tags, tag) {
			return false
		}
	}
	return true
}

func readError(operation string, err error) error {
	if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return invalid(operation + ": resource or API is unavailable")
	}
	return fmt.Errorf("%s: %w", operation, err)
}
