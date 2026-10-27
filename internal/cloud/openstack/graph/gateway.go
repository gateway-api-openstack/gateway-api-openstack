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

package graph

import (
	"context"
	"fmt"
	"regexp"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
)

// GetGateway discovers the controller-owned Gateway infrastructure for read-only
// verification. It does not validate route resources or authorize a mutation.
func (p *Provider) GetGateway(
	ctx context.Context,
	resourceIdentity cloud.Identity,
) (result cloud.GatewayResult, found bool, retErr error) {
	defer func() {
		retErr = classifyOpenStackError(retErr)
	}()

	ctx, cancel := p.operationContext(ctx)
	defer cancel()

	identity, err := p.identity(resourceIdentity)
	if err != nil {
		return cloud.GatewayResult{}, false, err
	}
	loadBalancer, err := p.findGatewayLoadBalancer(ctx, identity)
	if err != nil || loadBalancer == nil {
		return cloud.GatewayResult{}, false, err
	}
	if !identity.MatchesGateway(loadBalancer.Tags, roleLoadBalancer) {
		return cloud.GatewayResult{}, false, fmt.Errorf("%w: load balancer %s has an incomplete or stale identity", cloud.ErrOwnershipConflict, loadBalancer.ID)
	}
	observation, err := p.observeLoadBalancerOnce(ctx, loadBalancer.ID)
	if err != nil {
		return cloud.GatewayResult{}, false, err
	}
	switch observation.phase {
	case loadBalancerPhaseAbsent:
		return cloud.GatewayResult{}, false, nil
	case loadBalancerPhasePending:
		return cloud.GatewayResult{Outcome: observation.outcome}, true, nil
	case loadBalancerPhaseActive:
		loadBalancer = observation.loadBalancer
	default:
		return cloud.GatewayResult{}, false, fmt.Errorf("observe Gateway load balancer: unknown internal phase %d", observation.phase)
	}
	if err := p.validateLoadBalancerProject(loadBalancer.ID, loadBalancer.ProjectID); err != nil {
		return cloud.GatewayResult{}, false, err
	}
	if !identity.MatchesGateway(loadBalancer.Tags, roleLoadBalancer) {
		return cloud.GatewayResult{}, false, fmt.Errorf("%w: load balancer %s changed immutable identity during observation", cloud.ErrOwnershipConflict, loadBalancer.ID)
	}
	if !loadBalancer.AdminStateUp {
		return cloud.GatewayProgressingResult("Octavia load balancer is administratively disabled", p.pollInterval), true, nil
	}

	listener, err := p.findManagedGatewayListener(ctx, identity, loadBalancer.ID)
	if err != nil {
		return cloud.GatewayResult{}, false, err
	}
	if listener == nil {
		return cloud.GatewayProgressingResult("Octavia listener has not been created", p.pollInterval), true, nil
	}
	if listener.Protocol != string(listeners.ProtocolHTTP) {
		return cloud.GatewayResult{}, false, fmt.Errorf("%w: listener %s has protocol %s", cloud.ErrOwnershipConflict, listener.ID, listener.Protocol)
	}
	if !listener.AdminStateUp {
		return cloud.GatewayProgressingResult("Octavia listener is administratively disabled", p.pollInterval), true, nil
	}

	state := cloud.GatewayState{
		LoadBalancerID: loadBalancer.ID,
		VIPPortID:      loadBalancer.VipPortID,
		VIPAddress:     loadBalancer.VipAddress,
		ListenerID:     listener.ID,
	}
	floatingIP, err := p.findGatewayFloatingIP(ctx, identity, loadBalancer.VipPortID, loadBalancer.VipAddress)
	if err != nil {
		return cloud.GatewayResult{}, false, err
	}
	if floatingIP != nil {
		state.FloatingIPID = floatingIP.ID
		if floatingIP.PortID == "" {
			return cloud.GatewayResult{State: state, Outcome: p.progressingOutcome("Waiting for Floating IP association")}, true, nil
		}
		state.FloatingIPAddress = floatingIP.FloatingIP
	}
	return cloud.GatewayReadyResult(state), true, nil
}

func (p *Provider) findManagedGatewayListener(
	ctx context.Context,
	identity Identity,
	loadBalancerID string,
) (*listeners.Listener, error) {
	listenerList, err := p.octavia.ListListeners(ctx, listeners.ListOpts{
		LoadbalancerID: loadBalancerID,
		ProjectID:      p.projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("list Gateway listeners: %w", err)
	}
	var owned *listeners.Listener
	for index := range listenerList {
		listener := &listenerList[index]
		if err := p.validateOptionalProject("listener", listener.ID, listener.ProjectID); err != nil {
			return nil, err
		}
		if !identity.MatchesGatewayDiscovery(listener.Tags, roleListener) || !identity.MatchesGateway(listener.Tags, roleListener) {
			return nil, fmt.Errorf("%w: load balancer %s contains listener %s with an incomplete or foreign identity", cloud.ErrOwnershipConflict, loadBalancerID, listener.ID)
		}
		if owned != nil {
			return nil, fmt.Errorf("%w: found multiple listeners for one Gateway", cloud.ErrOwnershipConflict)
		}
		for _, parent := range listener.Loadbalancers {
			if parent.ID != loadBalancerID {
				return nil, fmt.Errorf("%w: listener %s reports load balancer %s instead of %s", cloud.ErrOwnershipConflict, listener.ID, parent.ID, loadBalancerID)
			}
		}
		owned = listener
	}
	return owned, nil
}

func (p *Provider) findGatewayFloatingIP(ctx context.Context, identity Identity, vipPortID, vipAddress string) (*floatingips.FloatingIP, error) {
	// Neutron offers no description-prefix filter. An exact filter would miss
	// resources carrying a previous release version in the trace suffix.
	// Filtering by VIP port would also hide a detached owned address and could
	// authorize a duplicate allocation. Inspect the complete project inventory.
	items, err := p.neutron.ListFloatingIPs(ctx, floatingips.ListOpts{ProjectID: p.projectID})
	if err != nil {
		return nil, fmt.Errorf("discover Gateway Floating IPs: %w", err)
	}
	var owned *floatingips.FloatingIP
	for index := range items {
		item := &items[index]
		matches := identity.MatchesGatewayDescription(item.Description, roleFloatingIP)
		if !matches && (vipPortID == "" || item.PortID != vipPortID) {
			continue
		}
		if err := p.validateFloatingIPProject(item.ID, item.ProjectID, item.TenantID); err != nil {
			return nil, err
		}
		if !matches {
			return nil, fmt.Errorf("%w: Gateway VIP port has an unmanaged Floating IP", cloud.ErrOwnershipConflict)
		}
		if item.ID == "" || item.TenantID != "" && item.TenantID != p.projectID {
			return nil, fmt.Errorf("%w: Gateway Floating IP has incomplete or inconsistent identity", cloud.ErrOwnershipConflict)
		}
		if item.PortID != "" && (item.PortID != vipPortID || vipAddress == "" || item.FixedIP != vipAddress) ||
			item.PortID == "" && (item.FixedIP != "" || item.RouterID != "") {
			return nil, fmt.Errorf("%w: Gateway Floating IP has an unexpected association", cloud.ErrOwnershipConflict)
		}
		if owned != nil {
			return nil, fmt.Errorf("%w: Gateway has multiple Floating IPs", cloud.ErrOwnershipConflict)
		}
		owned = item
	}
	return owned, nil
}

func (p *Provider) enableLoadBalancer(ctx context.Context, loadBalancerID string) error {
	if _, err := p.octavia.UpdateLoadBalancer(ctx, loadBalancerID, loadbalancers.UpdateOpts{
		AdminStateUp: boolPointer(true),
	}); err != nil {
		return fmt.Errorf("enable load balancer: %w", classifyOctaviaMutationError(err))
	}
	return nil
}

func (p *Provider) enableListener(ctx context.Context, listenerID string) error {
	if _, err := p.octavia.UpdateListener(ctx, listenerID, listeners.UpdateOpts{
		AdminStateUp: boolPointer(true),
	}); err != nil {
		return fmt.Errorf("enable listener: %w", classifyOctaviaMutationError(err))
	}
	return nil
}

// Compile the expression once because resource names are normalized on every
// reconciliation.
var invalidOpenStackResourceNameCharacters = regexp.MustCompile(`[^A-Za-z0-9-]+`)

func resourceName(identity cloud.Identity, role string) string {
	name := fmt.Sprintf(
		"%s-%s-%s-%s",
		openStackResourceNamePrefix,
		identity.GatewayNamespace,
		identity.GatewayName,
		role,
	)
	name = invalidOpenStackResourceNameCharacters.ReplaceAllString(name, "-")

	if len(name) > 200 {
		return name[:200]
	}
	return name
}
