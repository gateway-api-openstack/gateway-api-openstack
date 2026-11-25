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

package controllertest

import (
	"context"
	"fmt"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

// GraphProvider records complete writer requests, including failed requests.
// Tests supply Respond for non-ready outcomes and inspect Calls after reconciliation.
// Use one instance per test and treat recorded inputs as immutable.
// It does not simulate cloud mutations or enforce provider ownership rules.
type GraphProvider struct {
	Calls   []cloud.GatewayGraphSpec
	Respond func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error)
}

// ReconcileGatewayGraph records the request before returning the configured response.
func (p *GraphProvider) ReconcileGatewayGraph(ctx context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	p.Calls = append(p.Calls, spec)
	if p.Respond != nil {
		return p.Respond(ctx, spec)
	}
	return ReadyGraphResult(spec), nil
}

// GatewaySpecs returns the desired Gateway components of recorded Ensure requests.
func (p *GraphProvider) GatewaySpecs() []cloud.GatewaySpec {
	var specs []cloud.GatewaySpec
	for _, call := range p.Calls {
		if call.Gateway.Disposition == cloud.GraphEnsure {
			specs = append(specs, call.Gateway.Spec)
		}
	}
	return specs
}

// RouteSpecs returns the desired Route components of recorded Ensure requests.
func (p *GraphProvider) RouteSpecs() []cloud.RouteSpec {
	var specs []cloud.RouteSpec
	for _, call := range p.Calls {
		for _, route := range call.Routes {
			if route.Disposition == cloud.GraphEnsure {
				specs = append(specs, route.Spec)
			}
		}
	}
	return specs
}

// DeletedGateways returns the identities of recorded Gateway Delete requests.
func (p *GraphProvider) DeletedGateways() []cloud.Identity {
	var identities []cloud.Identity
	for _, call := range p.Calls {
		if call.Gateway.Disposition == cloud.GraphDelete {
			identities = append(identities, call.Identity)
		}
	}
	return identities
}

// DeletedRoutes returns the identities of recorded Route Delete requests.
func (p *GraphProvider) DeletedRoutes() []cloud.Identity {
	var identities []cloud.Identity
	for _, call := range p.Calls {
		for _, route := range call.Routes {
			if route.Disposition == cloud.GraphDelete {
				identities = append(identities, route.Spec.Identity)
			}
		}
	}
	return identities
}

// ReadyGraphResult builds a synthetic ready response for the complete request.
// Ensure results retain the requested listener keys and rule indexes. A preserved
// Gateway uses the supplied Route observations. Other results contain no resource IDs.
func ReadyGraphResult(spec cloud.GatewayGraphSpec) cloud.GatewayGraphResult {
	result := cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome(), Gateway: cloud.GatewayReadyResult(cloud.GatewayState{})}
	switch spec.Gateway.Disposition {
	case cloud.GraphEnsure:
		result.Gateway.State = cloud.GatewayState{LoadBalancerID: "lb-1", VIPAddress: "192.0.2.10"}
		for i, listener := range spec.Gateway.Spec.Listeners {
			result.Gateway.State.Listeners = append(result.Gateway.State.Listeners, cloud.ListenerState{Key: listener.Key, ID: fmt.Sprintf("listener-%d", i+1)})
		}
	case cloud.GraphPreserve:
		for _, route := range spec.Routes {
			if route.Spec.Gateway.LoadBalancerID != "" {
				result.Gateway.State = route.Spec.Gateway
				break
			}
		}
	}
	for _, route := range spec.Routes {
		component := cloud.RouteReadyResult(cloud.RouteState{})
		if route.Disposition == cloud.GraphEnsure {
			for _, rule := range route.Spec.Rules {
				component.State.Rules = append(component.State.Rules, cloud.RouteRuleState{Index: rule.Index, PoolID: fmt.Sprintf("pool-%d", rule.Index+1)})
			}
		}
		result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: route.Spec.Identity, Result: component})
	}
	return result
}
