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
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func TestGraphProviderRecordsFailedRequestsWithoutReplacingPartialResults(t *testing.T) {
	identity := cloud.Identity{GatewayUID: "gateway-uid", RouteUID: "route-uid"}
	spec := cloud.GatewayGraphSpec{
		Gateway: cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure},
		Routes:  []cloud.RouteGraphNode{{Disposition: cloud.GraphEnsure, Spec: cloud.RouteSpec{Identity: identity}}},
	}
	cause := errors.New("provider failed after observation")
	wantErr := &cloud.GraphError{Identity: identity, Err: cause}
	want := cloud.GatewayGraphResult{
		Outcome: cloud.ProgressingOutcome("Waiting for pool", time.Second),
		Gateway: cloud.GatewayReadyResult(cloud.GatewayState{LoadBalancerID: "observed-lb"}),
		Routes:  []cloud.RouteGraphResult{{Identity: identity, Result: cloud.RouteResult{State: cloud.RouteState{Rules: []cloud.RouteRuleState{{Index: 2, PoolID: "observed-pool"}}}}}},
	}
	provider := &GraphProvider{}
	ctx := t.Context()
	provider.Respond = func(received context.Context, request cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
		if received != ctx || !reflect.DeepEqual(request, spec) || !reflect.DeepEqual(provider.Calls, []cloud.GatewayGraphSpec{spec}) {
			t.Fatal("provider did not record the complete request before responding")
		}
		return want, wantErr
	}
	got, err := provider.ReconcileGatewayGraph(ctx, spec)
	if !reflect.DeepEqual(got, want) || err != wantErr || !errors.Is(err, cause) {
		t.Fatalf("response = %#v, %v, want unchanged partial result and graph error", got, err)
	}
	other := &GraphProvider{}
	if _, err := other.ReconcileGatewayGraph(ctx, cloud.GatewayGraphSpec{}); err != nil {
		t.Fatal(err)
	}
	if len(other.Calls) != 1 || !reflect.DeepEqual(provider.Calls, []cloud.GatewayGraphSpec{spec}) {
		t.Fatal("provider instances shared recorded requests")
	}
}

func TestReadyGraphResponseIncludesEveryRequestedComponent(t *testing.T) {
	first := cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 8080}
	second := cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 9090}
	ensured := cloud.Identity{GatewayUID: "gateway-uid", RouteUID: "ensured-route"}
	deleted := cloud.Identity{GatewayUID: "gateway-uid", RouteUID: "deleted-route"}
	preserved := cloud.Identity{GatewayUID: "gateway-uid", RouteUID: "preserved-route"}
	spec := cloud.GatewayGraphSpec{
		Identity: cloud.Identity{GatewayUID: "gateway-uid"},
		Gateway:  cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure, Spec: cloud.GatewaySpec{Listeners: []cloud.ListenerSpec{{Key: first}, {Key: second}}}},
		Routes: []cloud.RouteGraphNode{
			{Disposition: cloud.GraphEnsure, Spec: cloud.RouteSpec{Identity: ensured, Rules: []cloud.RouteRuleSpec{{Index: 2}, {Index: 5}}}},
			{Disposition: cloud.GraphDelete, Spec: cloud.RouteSpec{Identity: deleted}},
			{Disposition: cloud.GraphPreserve, Spec: cloud.RouteSpec{Identity: preserved}},
		},
	}
	provider := &GraphProvider{}
	result, err := provider.ReconcileGatewayGraph(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.GatewaySpecs()) != 1 || len(provider.RouteSpecs()) != 1 || !reflect.DeepEqual(provider.DeletedRoutes(), []cloud.Identity{deleted}) || len(provider.DeletedGateways()) != 0 {
		t.Fatalf("recorded requests lost or changed a component: %#v", provider.Calls)
	}
	listeners := result.Gateway.State.Listeners
	if len(listeners) != 2 || listeners[0].Key != first || listeners[1].Key != second {
		t.Fatalf("listener results = %#v, want both requested ports", listeners)
	}
	if len(result.Routes) != 3 || result.Routes[0].Identity != ensured || result.Routes[1].Identity != deleted || result.Routes[2].Identity != preserved {
		t.Fatalf("route results = %#v, want all request identities", result.Routes)
	}
	rules := result.Routes[0].Result.State.Rules
	if len(rules) != 2 || rules[0].Index != 2 || rules[1].Index != 5 || len(result.Routes[1].Result.State.Rules) != 0 || len(result.Routes[2].Result.State.Rules) != 0 {
		t.Fatalf("route results lost rule indexes or invented deleted/preserved IDs: %#v", result.Routes)
	}
	result.Gateway.State.Listeners[0].Key.Port = 1234
	result.Routes[0].Result.State.Rules[0].Index = 42
	next := ReadyGraphResult(spec)
	if next.Gateway.State.Listeners[0].Key != first || next.Routes[0].Result.State.Rules[0].Index != 2 {
		t.Fatal("ready responses shared state or changed requested inputs")
	}
}
