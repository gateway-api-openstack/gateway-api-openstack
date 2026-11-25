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
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/internal/controllertest"
)

type Config = controller.Config

const (
	testVIPSubnetID = controllertest.VIPSubnetID
)

const (
	providerFailureRequeue   = 2 * time.Minute
	ownershipConflictRequeue = 10 * time.Minute
)

var (
	Condition    = controller.Condition
	SetCondition = controller.SetCondition
)

type recordingProvider struct {
	controllertest.GraphProvider
}

func singleRouteGraphResponse(t *testing.T, routeResult cloud.RouteResult, routeErr error) func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	t.Helper()
	return func(_ context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
		t.Helper()
		if len(spec.Routes) != 1 || spec.Routes[0].Disposition == cloud.GraphPreserve {
			t.Fatalf("graph request = %#v, want one ensured or deleted Route", spec)
		}
		result := controllertest.ReadyGraphResult(spec)
		result.Routes[0].Result = routeResult
		if routeErr != nil {
			return result, &cloud.GraphError{Identity: spec.Routes[0].Spec.Identity, Err: routeErr}
		}
		result.Outcome = routeResult.Outcome
		return result, nil
	}
}

func testConfig() Config {
	return Config{
		ControllerName:          "example.com/gateway-api-openstack",
		ControllerVersion:       "test",
		OpenStackProjectID:      "project-a",
		ClusterID:               "cluster-a",
		OpenStackResyncInterval: time.Minute,
	}
}

func gatewayBindingAnnotations(cfg Config, _ string) map[string]string {
	return map[string]string{
		cfg.GatewayClusterIDAnnotation(): cfg.ClusterID,
		cfg.GatewayProjectIDAnnotation(): cfg.OpenStackProjectID,
	}
}

func routeHasBindingAnnotations(cfg Config, route *gatewayv1.HTTPRoute) bool {
	return route.Annotations[cfg.RouteBindingsAnnotation()] != ""
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	return controllertest.Scheme(t)
}

func indexedFakeClientBuilder(scheme *runtime.Scheme, config Config) *controllertest.ClientBuilder {
	return controllertest.NewClientBuilder(scheme, config)
}

func rawIndexedFakeClientBuilder(scheme *runtime.Scheme, config Config) *fake.ClientBuilder {
	return controllertest.RawClientBuilder(scheme, config)
}

func supportedGatewayAPICRDs() []client.Object {
	return controllertest.SupportedGatewayAPICRDs()
}

func testBoundTopology() classconfig.Resolved {
	return controllertest.BoundTopology()
}

func completeGatewayTopologyFixture(cfg Config, gateway *gatewayv1.Gateway) {
	controllertest.CompleteGatewayTopology(cfg, gateway)
}

func routeBindingJSON(cfg Config, identity cloud.Identity, lifecycle controller.RouteLifecycle, cleanupFailure string) string {
	identity.Controller = string(cfg.ControllerName)
	identity.ControllerVersion = cfg.ControllerVersion
	encoded, err := json.Marshal([]controller.RouteBinding{{Identity: identity, Lifecycle: lifecycle, CleanupFailure: cleanupFailure}})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func bindingFixture(tb testing.TB, cfg Config, annotations map[string]string) controller.RouteBinding {
	tb.Helper()
	encoded := annotations[cfg.RouteBindingsAnnotation()]
	if encoded == "" {
		return controller.RouteBinding{}
	}
	var bindings []controller.RouteBinding
	if err := json.Unmarshal([]byte(encoded), &bindings); err != nil {
		tb.Fatalf("decode route binding fixture: %v", err)
	}
	if len(bindings) != 1 {
		tb.Fatalf("route binding fixture contains %d bindings, want 1", len(bindings))
	}
	return bindings[0]
}

func updateBindingFixture(tb testing.TB, cfg Config, annotations map[string]string, update func(*controller.RouteBinding)) {
	tb.Helper()
	binding := bindingFixture(tb, cfg, annotations)
	update(&binding)
	annotations[cfg.RouteBindingsAnnotation()] = routeBindingJSON(cfg, binding.Identity, binding.Lifecycle, binding.CleanupFailure)
}

func (*recordingProvider) ResolveTopology(_ context.Context, request cloud.TopologyRequest) (cloud.Topology, error) {
	topology := cloud.Topology{MemberMode: cloud.MemberModeNodePort, VIPSubnetID: request.VIPSubnet.ID, MemberSubnetID: request.MemberSubnet.ID, FlavorID: request.FlavorID, AvailabilityZone: request.AvailabilityZone}
	if request.ExternalNetwork != nil {
		topology.ExternalNetworkID = request.ExternalNetwork.ID
	}
	return topology, nil
}
