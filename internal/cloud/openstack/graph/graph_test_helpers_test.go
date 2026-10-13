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
	"net/http"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

// Focused component regressions use the same complete graph entry point as the
// controller. The helpers supply the unaffected components explicitly.
func newTestProvider(t *testing.T, clients ServiceClients, config ProviderConfig) *Provider {
	t.Helper()
	if clients.Network == nil {
		clients.Network = safetyServiceClient(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodGet || request.URL.Path != "/floatingips" {
				t.Errorf("unexpected network request: %s %s", request.Method, request.URL)
				http.Error(w, "unexpected request", http.StatusNotFound)
				return
			}
			safetyWriteJSON(t, w, map[string]any{"floatingips": []any{}})
		}))
	}
	return NewProvider(clients, config)
}

func gatewayIdentity(id cloud.Identity) cloud.Identity {
	id.RouteNamespace, id.RouteName, id.RouteUID = "", "", ""
	return id
}

func (p *Provider) EnsureGateway(ctx context.Context, spec cloud.GatewaySpec) (cloud.GatewayResult, error) {
	id := spec.Identity
	spec.Identity = gatewayIdentity(id)
	graph := cloud.GatewayGraphSpec{Identity: spec.Identity, Gateway: cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure, Spec: spec}}
	if id.RouteUID != "" {
		graph.Routes = []cloud.RouteGraphNode{{Disposition: cloud.GraphPreserve, Spec: cloud.RouteSpec{Identity: id}}}
	}
	result, err := p.ReconcileGatewayGraph(ctx, graph)
	result.Gateway.Outcome = result.Outcome
	return result.Gateway, err
}

func (p *Provider) EnsureRoute(ctx context.Context, spec cloud.RouteSpec) (cloud.RouteResult, error) {
	id := gatewayIdentity(spec.Identity)
	requirements := spec.GatewayRequirements
	graph := cloud.GatewayGraphSpec{Identity: id, Gateway: cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure, Spec: cloud.GatewaySpec{
		Identity: id, Provider: requirements.Provider, VIPSubnetID: requirements.VIPSubnetID, ExternalNetworkID: requirements.ExternalNetworkID,
		FlavorID: requirements.FlavorID, AvailabilityZone: requirements.AvailabilityZone, ListenerPort: requirements.ListenerPort,
	}}, Routes: []cloud.RouteGraphNode{{Disposition: cloud.GraphEnsure, Spec: spec}}}
	result, err := p.ReconcileGatewayGraph(ctx, graph)
	if len(result.Routes) == 0 {
		return cloud.RouteResult{Outcome: result.Outcome}, err
	}
	route := result.Routes[0].Result
	route.Outcome = result.Outcome
	return route, err
}

func (p *Provider) DeleteGateway(ctx context.Context, id cloud.Identity) (cloud.Outcome, error) {
	result, err := p.ReconcileGatewayGraph(ctx, cloud.GatewayGraphSpec{Identity: gatewayIdentity(id), Gateway: cloud.GatewayGraphNode{Disposition: cloud.GraphDelete}})
	return result.Outcome, err
}

func (p *Provider) DeleteRoute(ctx context.Context, id cloud.Identity) (cloud.Outcome, error) {
	result, err := p.ReconcileGatewayGraph(ctx, cloud.GatewayGraphSpec{Identity: gatewayIdentity(id),
		Gateway: cloud.GatewayGraphNode{Disposition: cloud.GraphPreserve},
		Routes:  []cloud.RouteGraphNode{{Disposition: cloud.GraphDelete, Spec: cloud.RouteSpec{Identity: id}}},
	})
	return result.Outcome, err
}
