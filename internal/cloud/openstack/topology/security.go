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
	"fmt"
	"net/netip"
	"slices"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
)

func (r *Resolver) resolveSecurityGroups(ctx context.Context, references []cloud.ResourceReference) ([]string, error) {
	if len(references) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(references))
	for _, reference := range references {
		group, err := r.resolveSecurityGroup(ctx, reference)
		if err != nil {
			return nil, err
		}
		ids = append(ids, group.ID)
	}
	slices.Sort(ids)
	if len(slices.Compact(ids)) != len(references) {
		return nil, invalid("security group references resolve to duplicate groups")
	}
	return ids, nil
}

func (r *Resolver) resolveSecurityGroup(ctx context.Context, reference cloud.ResourceReference) (*groups.SecGroup, error) {
	if err := validateReference(reference); err != nil {
		return nil, err
	}
	var group *groups.SecGroup
	if reference.ID != "" {
		var err error
		group, err = groups.Get(ctx, r.clients.Network, reference.ID).Extract()
		if err != nil {
			return nil, readError("get security group", err)
		}
	} else {
		pages, err := groups.List(r.clients.Network, groups.ListOpts{
			ProjectID: r.clients.ProjectID, Name: reference.Name, Tags: tagFilter(reference.Tags),
		}).AllPages(ctx)
		if err != nil {
			return nil, readError("list security groups", err)
		}
		matches, err := groups.ExtractGroups(pages)
		if err != nil {
			return nil, fmt.Errorf("extract security groups: %w", err)
		}
		if len(matches) != 1 {
			return nil, invalid("security group reference must resolve to exactly one resource")
		}
		group = &matches[0]
	}
	if group == nil || group.ID == "" || !referenceMatches(reference, group.ID, group.Name, group.Tags) {
		return nil, invalid("resolved security group does not match its reference")
	}
	if group.ProjectID != r.clients.ProjectID || group.TenantID != "" && group.TenantID != r.clients.ProjectID {
		return nil, invalid("security group does not belong to the authenticated project")
	}
	return group, nil
}

// ValidateGatewaySecurity checks referenced access without changing groups or
// ports. Call it only for Ensure after validating the observed ownership graph.
// Rule coverage establishes prerequisites, not end-to-end reachability.
func (r *Resolver) ValidateGatewaySecurity(ctx context.Context, gateway cloud.GatewaySpec, routes []cloud.RouteSpec) (retErr error) {
	// Security rules and port associations have no Kubernetes watch. Keep
	// observing failed prerequisites so an operator's repair can unblock them.
	defer func() {
		if errors.Is(retErr, cloud.ErrTerminalValidation) {
			var component *cloud.GraphError
			if errors.As(retErr, &component) {
				retErr = &cloud.GraphError{Identity: component.Identity, Err: cloud.NewProviderError(cloud.ErrorCategoryDependencyNotReady, component.Err)}
			} else {
				retErr = cloud.NewProviderError(cloud.ErrorCategoryDependencyNotReady, retErr)
			}
		}
	}()
	hasReferences := len(gateway.VIPSecurityGroupIDs) != 0
	for _, route := range routes {
		hasReferences = hasReferences || len(route.BackendSecurityGroupIDs) != 0
	}
	if !hasReferences {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.operationTimeout)
	defer cancel()

	// Reads are reused within this snapshot only. Referenced rules can change
	// independently of Kubernetes inputs and must be observed on the next run.
	observed := make(map[string]*groups.SecGroup)
	readGroups := func(ids []string) ([]groups.SecGroup, error) {
		result := make([]groups.SecGroup, 0, len(ids))
		for _, id := range ids {
			group := observed[id]
			if group == nil {
				var err error
				group, err = r.resolveSecurityGroup(ctx, cloud.ResourceReference{ID: id})
				if err != nil {
					return nil, err
				}
				observed[id] = group
			}
			result = append(result, *group)
		}
		return result, nil
	}
	if len(gateway.VIPSecurityGroupIDs) != 0 {
		request := cloud.TopologyRequest{FlavorID: gateway.FlavorID, AvailabilityZone: gateway.AvailabilityZone}
		for _, id := range gateway.VIPSecurityGroupIDs {
			request.VIPSecurityGroups = append(request.VIPSecurityGroups, cloud.ResourceReference{ID: id})
		}
		if err := r.checkCapabilities(ctx, request); err != nil {
			return err
		}
		subnet, err := r.resolveSubnet(ctx, cloud.ResourceReference{ID: gateway.VIPSubnetID})
		if err != nil {
			return err
		}
		frontend, err := readGroups(gateway.VIPSecurityGroupIDs)
		if err != nil {
			return err
		}
		if !allowsTCPIngress(frontend, subnet.IPVersion, gateway.ListenerPort) {
			return invalid("referenced frontend groups do not allow the listener protocol and port for the VIP IP family")
		}
	}
	for _, route := range routes {
		if len(route.BackendSecurityGroupIDs) == 0 {
			continue
		}
		backend, err := readGroups(route.BackendSecurityGroupIDs)
		if err != nil {
			return &cloud.GraphError{Identity: route.Identity, Err: err}
		}
		for _, member := range route.Members {
			address, err := netip.ParseAddr(member.Address)
			if err != nil || address.Is4In6() {
				return &cloud.GraphError{Identity: route.Identity, Err: invalid("backend member address is invalid")}
			}
			family := 6
			if address.Is4() {
				family = 4
			}
			if !allowsTCPIngress(backend, family, member.Port) {
				return &cloud.GraphError{Identity: route.Identity, Err: invalid("referenced backend groups do not allow the member protocol and port for its IP family")}
			}
			if err := r.ValidateMemberPort(ctx, member.ProviderID, member.Address, route.MemberSubnetID, route.BackendSecurityGroupIDs); err != nil {
				return &cloud.GraphError{Identity: route.Identity, Err: err}
			}
		}
	}
	return nil
}

func allowsTCPIngress(securityGroups []groups.SecGroup, family, port int) bool {
	etherType := "IPv6"
	if family == 4 {
		etherType = "IPv4"
	}
	for _, group := range securityGroups {
		for _, rule := range group.Rules {
			if rule.Direction != "ingress" || rule.EtherType != etherType {
				continue
			}
			if rule.Protocol != "" && rule.Protocol != "tcp" && rule.Protocol != "6" {
				continue
			}
			if rule.RemoteIPPrefix != "" {
				prefix, err := netip.ParsePrefix(rule.RemoteIPPrefix)
				if err != nil || prefix.Addr().Is4In6() || prefix.Addr().Is4() != (family == 4) {
					continue
				}
			}
			if rule.PortRangeMin == 0 && rule.PortRangeMax == 0 || rule.PortRangeMin <= port && port <= rule.PortRangeMax {
				return true
			}
		}
	}
	return false
}
