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

package gateway

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func (r *Reconciler) ensureGateway(ctx context.Context, expected *gatewayv1.Gateway) (cloud.GatewayGraphResult, error) {
	if r.Writer == nil {
		return cloud.GatewayGraphResult{}, fmt.Errorf("gateway graph writer is required")
	}
	return r.Writer.ReconcileGraph(ctx, graph.Request{Identity: controller.GatewayIdentity(r.Config, expected), Action: graph.Ensure, Gateway: expected})
}

func (r *Reconciler) gatewaySpec(gateway *gatewayv1.Gateway, listeners []gatewayv1.Listener, topology classconfig.Resolved) cloud.GatewaySpec {
	physical := make([]cloud.ListenerSpec, 0, len(listeners))
	for _, listener := range listeners {
		physical = append(physical, cloud.ListenerSpec{Key: cloud.ListenerKey{Protocol: string(listener.Protocol), Port: int(listener.Port)}})
	}
	slices.SortFunc(physical, func(a, b cloud.ListenerSpec) int { return cmp.Compare(a.Key.String(), b.Key.String()) })
	physical = slices.Compact(physical)
	return cloud.GatewaySpec{
		Identity:            controller.GatewayIdentity(r.Config, gateway),
		Provider:            "amphora",
		VIPSubnetID:         topology.Topology.VIPSubnetID,
		ExternalNetworkID:   topology.Topology.ExternalNetworkID,
		Listeners:           physical,
		FlavorID:            topology.Topology.FlavorID,
		AvailabilityZone:    topology.Topology.AvailabilityZone,
		VIPSecurityGroupIDs: slices.Clone(topology.Topology.VIPSecurityGroupIDs),
	}
}
