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

// Package topology resolves existing infrastructure without provisioning or
// acquiring ownership of the resources it reads.
package topology

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/apierrors"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
)

// Resolver performs bounded, fresh reads for one class topology evaluation.
// It verifies API visibility and advertised features, not mutation permissions
// or connectivity from Amphora to a backend.
type Resolver struct {
	clients          clients.ServiceClients
	operationTimeout time.Duration
}

// NewResolver reuses the process-scoped authenticated clients.
func NewResolver(serviceClients clients.ServiceClients, operationTimeout time.Duration) *Resolver {
	return &Resolver{clients: serviceClients, operationTimeout: operationTimeout}
}

var _ cloud.TopologyResolver = (*Resolver)(nil)

// ResolveTopology returns exact IDs only after all references and capabilities
// are validated. A failed read never returns a partially resolved topology.
func (r *Resolver) ResolveTopology(ctx context.Context, request cloud.TopologyRequest) (result cloud.Topology, retErr error) {
	defer func() { retErr = apierrors.Classify(retErr) }()
	if r.clients.ProjectID == "" || r.clients.Network == nil || r.clients.LoadBalancer == nil || r.operationTimeout <= 0 {
		return cloud.Topology{}, invalid("topology resolver requires authenticated service clients and a positive timeout")
	}
	if err := validateRequest(request); err != nil {
		return cloud.Topology{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.operationTimeout)
	defer cancel()
	if err := r.checkCapabilities(ctx, request); err != nil {
		return cloud.Topology{}, err
	}
	vip, err := r.resolveSubnet(ctx, request.VIPSubnet)
	if err != nil {
		return cloud.Topology{}, fmt.Errorf("resolve VIP subnet: %w", err)
	}
	member := vip
	if !sameReference(request.VIPSubnet, request.MemberSubnet) {
		member, err = r.resolveSubnet(ctx, request.MemberSubnet)
		if err != nil {
			return cloud.Topology{}, fmt.Errorf("resolve member subnet: %w", err)
		}
	}
	if vip.IPVersion != member.IPVersion {
		return cloud.Topology{}, invalid("VIP and member subnets must use the same IP version")
	}
	// Reuse parent network reads only within this snapshot. Names and tags are
	// resolved again on the next class evaluation.
	observedNetworks := make(map[string]*networks.Network, 2)
	for _, subnet := range []*subnets.Subnet{vip, member} {
		network := observedNetworks[subnet.NetworkID]
		if network == nil {
			network, err = networks.Get(ctx, r.clients.Network, subnet.NetworkID).Extract()
			if err != nil {
				return cloud.Topology{}, readError("get subnet network", err)
			}
			if network == nil || network.ID != subnet.NetworkID || !network.AdminStateUp {
				return cloud.Topology{}, invalid("subnet network is missing, inconsistent, or disabled")
			}
			observedNetworks[subnet.NetworkID] = network
		}
		if !slices.Contains(network.Subnets, subnet.ID) {
			return cloud.Topology{}, invalid("subnet is not listed by its parent network")
		}
	}
	result = cloud.Topology{
		VIPSubnetID: vip.ID, MemberSubnetID: member.ID,
		FlavorID: request.FlavorID, AvailabilityZone: request.AvailabilityZone,
	}
	if request.ExternalNetwork != nil {
		if vip.IPVersion != 4 {
			return cloud.Topology{}, invalid("Floating IP placement requires an IPv4 VIP subnet")
		}
		networkID, err := r.resolveExternalNetwork(ctx, *request.ExternalNetwork)
		if err != nil {
			return cloud.Topology{}, fmt.Errorf("resolve external network: %w", err)
		}
		result.ExternalNetworkID = networkID
	}
	result.VIPSecurityGroupIDs, err = r.resolveSecurityGroups(ctx, request.VIPSecurityGroups)
	if err != nil {
		return cloud.Topology{}, fmt.Errorf("resolve frontend security groups: %w", err)
	}
	result.BackendSecurityGroupIDs, err = r.resolveSecurityGroups(ctx, request.BackendSecurityGroups)
	if err != nil {
		return cloud.Topology{}, fmt.Errorf("resolve backend security groups: %w", err)
	}
	return result, nil
}

func validateRequest(request cloud.TopologyRequest) error {
	for _, reference := range []cloud.ResourceReference{request.VIPSubnet, request.MemberSubnet} {
		if err := validateReference(reference); err != nil {
			return err
		}
	}
	if request.ExternalNetwork != nil {
		if err := validateReference(*request.ExternalNetwork); err != nil {
			return err
		}
	}
	for _, references := range [][]cloud.ResourceReference{request.VIPSecurityGroups, request.BackendSecurityGroups} {
		if len(references) > 16 {
			return invalid("security configuration allows at most 16 group references")
		}
		for _, reference := range references {
			if err := validateReference(reference); err != nil {
				return err
			}
		}
	}
	if strings.ContainsAny(request.FlavorID, "/?#") || strings.TrimSpace(request.FlavorID) != request.FlavorID {
		return invalid("flavor ID is invalid")
	}
	if strings.TrimSpace(request.AvailabilityZone) != request.AvailabilityZone {
		return invalid("availability zone name is invalid")
	}
	return nil
}

func validateReference(reference cloud.ResourceReference) error {
	count := 0
	if reference.ID != "" {
		count++
		if strings.ContainsAny(reference.ID, "/?#") || strings.TrimSpace(reference.ID) != reference.ID {
			return invalid("resource ID is invalid")
		}
	}
	if reference.Name != "" {
		count++
		if strings.TrimSpace(reference.Name) != reference.Name {
			return invalid("resource name is invalid")
		}
	}
	if len(reference.Tags) > 0 {
		count++
		seen := make(map[string]struct{}, len(reference.Tags))
		for _, tag := range reference.Tags {
			if tag == "" || strings.Contains(tag, ",") || strings.TrimSpace(tag) != tag {
				return invalid("resource tags must be nonempty individual tags")
			}
			if _, duplicate := seen[tag]; duplicate {
				return invalid("resource tags must be unique")
			}
			seen[tag] = struct{}{}
		}
	}
	if count != 1 {
		return invalid("resource reference requires exactly one of ID, name, or tags")
	}
	return nil
}

func sameReference(left, right cloud.ResourceReference) bool {
	return left.ID == right.ID && left.Name == right.Name && tagFilter(left.Tags) == tagFilter(right.Tags)
}

func tagFilter(tags []string) string {
	ordered := slices.Clone(tags)
	slices.Sort(ordered)
	return strings.Join(ordered, ",")
}

func invalid(message string) error {
	return cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, fmt.Errorf("%s", message))
}
