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

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
)

func (p *Provider) executeGatewayDeletionStep(ctx context.Context, step gatewayDeletionStep) error {
	deleteErr := p.deleteGatewayResource(ctx, step)
	if deleteErr != nil && !isNotFound(deleteErr) {
		return fmt.Errorf("delete %s %s: %w", step.resource, step.resourceID, classifyOctaviaMutationError(deleteErr))
	}
	return nil
}

func (p *Provider) deleteGatewayResource(ctx context.Context, step gatewayDeletionStep) error {
	switch step.resource {
	case gatewayDeletionL7Rule:
		return p.octavia.DeleteRule(ctx, step.parentID, step.resourceID)
	case gatewayDeletionL7Policy:
		return p.octavia.DeletePolicy(ctx, step.resourceID)
	case gatewayDeletionListener:
		return p.octavia.DeleteListener(ctx, step.resourceID)
	case gatewayDeletionMonitor:
		return p.octavia.DeleteMonitor(ctx, step.resourceID)
	case gatewayDeletionMember:
		return p.octavia.DeleteMember(ctx, step.parentID, step.resourceID)
	case gatewayDeletionPool:
		return p.octavia.DeletePool(ctx, step.resourceID)
	default:
		return fmt.Errorf("unsupported Gateway deletion resource %q", step.resource)
	}
}

func (p *Provider) findGatewayLoadBalancer(ctx context.Context, identity Identity) (*loadbalancers.LoadBalancer, error) {
	tags, err := identity.GatewayDiscoveryTags(roleLoadBalancer)
	if err != nil {
		return nil, err
	}
	items, err := p.octavia.ListLoadBalancers(ctx, loadbalancers.ListOpts{ProjectID: p.projectID, Tags: tags})
	if err != nil {
		return nil, fmt.Errorf("list managed load balancers: %w", err)
	}
	if len(items) > 1 {
		return nil, fmt.Errorf("%w: found %d load balancers for one Gateway", cloud.ErrOwnershipConflict, len(items))
	}
	if len(items) == 0 {
		return nil, nil
	}
	if err := p.validateLoadBalancerProject(items[0].ID, items[0].ProjectID); err != nil {
		return nil, err
	}
	return &items[0], nil
}
