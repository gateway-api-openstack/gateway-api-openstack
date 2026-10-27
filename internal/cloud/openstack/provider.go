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

// Package openstack implements the cloud.GraphProvider boundary with Octavia and
// Neutron. Kubernetes types intentionally do not enter this package.
package openstack

import (
	"context"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/audit"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/graph"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/inventory"
)

// ProviderConfig bounds one provider call and controls how soon the
// controller observes an asynchronous Octavia operation again.
type ProviderConfig = graph.ProviderConfig

// Provider is the stable OpenStack facade used by the controller and the
// read-only ownership audit command.
type Provider struct {
	graph     *graph.Provider
	inventory *inventory.Scanner
}

// NewProvider creates an OpenStack provider. The clients are expected to have
// an Octavia microversion that supports tags (2.5 or later).
func NewProvider(clients ServiceClients, cfg ProviderConfig) *Provider {
	return &Provider{
		graph:     graph.NewProvider(clients, cfg),
		inventory: inventory.NewScanner(clients, cfg.OperationTimeout),
	}
}

var (
	_ cloud.GraphProvider = (*Provider)(nil)
	_ audit.Scanner       = (*Provider)(nil)
)

// ReconcileGatewayGraph reconciles one complete, validated Gateway graph.
func (p *Provider) ReconcileGatewayGraph(ctx context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	return p.graph.ReconcileGatewayGraph(ctx, spec)
}

// GetGateway delegates read-only Gateway discovery to the graph package.
func (p *Provider) GetGateway(ctx context.Context, identity cloud.Identity) (cloud.GatewayResult, bool, error) {
	return p.graph.GetGateway(ctx, identity)
}

// Scan delegates the read-only ownership inventory to the inventory package.
func (p *Provider) Scan(
	ctx context.Context,
	scope audit.Scope,
	records []audit.OwnershipRecord,
) (audit.Inventory, error) {
	return p.inventory.Scan(ctx, scope, records)
}
