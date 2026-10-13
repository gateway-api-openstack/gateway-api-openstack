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

package httproute

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestHTTPRouteRequiresItsComponentOutcome(t *testing.T) {
	tests := []struct {
		name      string
		result    func(cloud.Identity) cloud.GatewayGraphResult
		wantError bool
		wantState cloud.OutcomeState
	}{
		{name: "missing ready component", wantError: true, result: func(cloud.Identity) cloud.GatewayGraphResult {
			return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome()}
		}},
		{name: "missing progressing component", wantState: cloud.OutcomeProgressing, result: func(cloud.Identity) cloud.GatewayGraphResult {
			return cloud.GatewayGraphResult{Outcome: cloud.ProgressingOutcome("Waiting for listener", time.Second)}
		}},
		{name: "invalid outcome", wantError: true, result: func(id cloud.Identity) cloud.GatewayGraphResult {
			return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome(), Routes: []cloud.RouteGraphResult{{Identity: id}}}
		}},
		{name: "foreign identity", wantError: true, result: func(id cloud.Identity) cloud.GatewayGraphResult {
			id.GatewayUID = "other"
			return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome(), Routes: []cloud.RouteGraphResult{{Identity: id, Result: cloud.RouteReadyResult(cloud.RouteState{})}}}
		}},
		{name: "duplicate identity", wantError: true, result: func(id cloud.Identity) cloud.GatewayGraphResult {
			component := cloud.RouteGraphResult{Identity: id, Result: cloud.RouteReadyResult(cloud.RouteState{})}
			return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome(), Routes: []cloud.RouteGraphResult{component, component}}
		}},
		{name: "ready component", wantState: cloud.OutcomeReady, result: func(id cloud.Identity) cloud.GatewayGraphResult {
			return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome(), Routes: []cloud.RouteGraphResult{{Identity: id, Result: cloud.RouteReadyResult(cloud.RouteState{PoolID: "pool-1"})}}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			gateway := programmedTestGateway(cfg)
			class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
			route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
			route.UID = "route-uid"
			route.Generation = 1
			seed := &Reconciler{Config: cfg}
			seed.applyRouteBinding(route, gateway)
			service, endpoints, node := validNodePortBackend("default", "backend")
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithObjects(class, gateway, route, service, endpoints, node).Build()
			expectedIdentity := controller.RouteIdentity(cfg, gateway, route)
			reconciler := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Config: cfg, Coordinator: &graph.Coordinator{}, Writer: fixedRouteGraph{result: test.result(expectedIdentity)}}, nil)
			result, err := reconciler.ensureRouteGraph(context.Background(), route, gateway)
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v", err)
			}
			if err == nil && result.outcome.State != test.wantState {
				t.Fatalf("outcome=%#v", result.outcome)
			}
		})
	}
}

type fixedRouteGraph struct{ result cloud.GatewayGraphResult }

func (w fixedRouteGraph) ReconcileGraph(context.Context, graph.Request) (cloud.GatewayGraphResult, error) {
	return w.result, nil
}
