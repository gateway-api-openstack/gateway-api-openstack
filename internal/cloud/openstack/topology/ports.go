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
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/portsecurity"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
)

type memberPort struct {
	ports.Port
	portsecurity.PortSecurityExt
}

// ValidateMemberPort verifies one worker's exact port and referenced groups.
// It never changes a port, its security groups, or network topology.
func (r *Resolver) ValidateMemberPort(ctx context.Context, providerID, address, subnetID string, securityGroupIDs []string) error {
	serverID, err := serverIDFromProviderID(providerID, r.clients.Region)
	if err != nil {
		return err
	}
	selectedAddress, err := netip.ParseAddr(address)
	if err != nil || selectedAddress.Is4In6() || selectedAddress.IsUnspecified() || selectedAddress.IsMulticast() || subnetID == "" {
		return invalid("member port requires a valid address and subnet")
	}
	pages, err := ports.List(r.clients.Network, ports.ListOpts{
		ProjectID: r.clients.ProjectID, DeviceID: serverID,
		FixedIPs: []ports.FixedIPOpts{{IPAddress: address, SubnetID: subnetID}},
	}).AllPages(ctx)
	if err != nil {
		return readError("list member ports", err)
	}
	var matches []memberPort
	if err := ports.ExtractPortsInto(pages, &matches); err != nil {
		return fmt.Errorf("extract member ports: %w", err)
	}
	if len(matches) != 1 {
		return invalid("member must map to exactly one Neutron port")
	}
	port := matches[0]
	if port.ID == "" || port.ProjectID != r.clients.ProjectID || port.TenantID != "" && port.TenantID != r.clients.ProjectID ||
		port.DeviceID != serverID || !strings.HasPrefix(port.DeviceOwner, "compute:") || !port.AdminStateUp {
		return invalid("member port identity, ownership, or security state is invalid")
	}
	if len(securityGroupIDs) != 0 && !port.PortSecurityEnabled {
		return invalid("referenced backend security groups require port security")
	}
	found := false
	for _, fixedIP := range port.FixedIPs {
		parsed, err := netip.ParseAddr(fixedIP.IPAddress)
		if err == nil && parsed == selectedAddress && fixedIP.SubnetID == subnetID {
			found = true
			break
		}
	}
	if !found {
		return invalid("member port does not have the selected address and subnet")
	}
	for _, id := range securityGroupIDs {
		if !slices.Contains(port.SecurityGroups, id) {
			return invalid("member port is missing a referenced backend security group")
		}
	}
	return nil
}

func serverIDFromProviderID(providerID, region string) (string, error) {
	parsed, err := url.Parse(providerID)
	if err != nil || !strings.HasPrefix(providerID, "openstack://") || parsed.Scheme != "openstack" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.RawPath != "" || parsed.Opaque != "" || parsed.Host != "" && parsed.Host != region {
		return "", invalid("Node provider ID must select OpenStack in the configured region")
	}
	id := strings.TrimPrefix(parsed.Path, "/")
	parsedID, err := uuid.Parse(id)
	if err != nil || parsedID.String() != id {
		return "", invalid("Node provider ID must contain a canonical server UUID")
	}
	return id, nil
}
