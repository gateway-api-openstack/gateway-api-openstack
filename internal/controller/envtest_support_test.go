//go:build envtest

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

package controller_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	gatewaycontroller "github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/gateway"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
	httproutecontroller "github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/httproute"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type recordingProvider struct {
	gatewaySpecs     []cloud.GatewaySpec
	graphSpecs       []cloud.GatewayGraphSpec
	gatewayDeleteOut *cloud.Outcome
	deletedGateways  []cloud.Identity
}

func (p *recordingProvider) EnsureGateway(_ context.Context, spec cloud.GatewaySpec) (cloud.GatewayResult, error) {
	p.gatewaySpecs = append(p.gatewaySpecs, spec)
	return cloud.GatewayReadyResult(cloud.GatewayState{
		LoadBalancerID: "lb-1",
		VIPAddress:     "192.0.2.10",
		ListenerID:     "listener-1",
	}), nil
}

func (p *recordingProvider) GetGateway(context.Context, cloud.Identity) (cloud.GatewayResult, bool, error) {
	return cloud.GatewayReadyResult(cloud.GatewayState{
		LoadBalancerID: "lb-1",
		VIPAddress:     "192.0.2.10",
		ListenerID:     "listener-1",
	}), true, nil
}

func (p *recordingProvider) DeleteGateway(_ context.Context, identity cloud.Identity) (cloud.Outcome, error) {
	p.deletedGateways = append(p.deletedGateways, identity)
	if p.gatewayDeleteOut != nil {
		return *p.gatewayDeleteOut, nil
	}
	return cloud.ReadyOutcome(), nil
}

func (*recordingProvider) EnsureRoute(context.Context, cloud.RouteSpec) (cloud.RouteResult, error) {
	return cloud.RouteReadyResult(cloud.RouteState{PoolID: "pool-1"}), nil
}

func (*recordingProvider) DeleteRoute(context.Context, cloud.Identity) (cloud.Outcome, error) {
	return cloud.ReadyOutcome(), nil
}

func testConfig() controller.Config {
	return controller.Config{
		ControllerName:          "example.com/gateway-api-openstack",
		ControllerVersion:       "test",
		OpenStackProjectID:      "project-a",
		ClusterID:               "cluster-a",
		OpenStackResyncInterval: time.Minute,
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func (*recordingProvider) ResolveTopology(_ context.Context, request cloud.TopologyRequest) (cloud.Topology, error) {
	topology := cloud.Topology{VIPSubnetID: request.VIPSubnet.ID, MemberSubnetID: request.MemberSubnet.ID, FlavorID: request.FlavorID, AvailabilityZone: request.AvailabilityZone}
	if request.ExternalNetwork != nil {
		topology.ExternalNetworkID = request.ExternalNetwork.ID
	}
	return topology, nil
}

func newEnvtestGatewayReconciler(kubeClient client.Client, reader client.Reader, provider *recordingProvider, config controller.Config) *gatewaycontroller.Reconciler {
	coordinator := &graph.Coordinator{}
	gateway := &gatewaycontroller.Reconciler{Client: kubeClient, APIReader: reader, TopologyResolver: provider, Coordinator: coordinator, Config: config}
	route := &httproutecontroller.Reconciler{Client: kubeClient, APIReader: reader, Coordinator: coordinator, Config: config}
	writer := &graph.Writer{Coordinator: coordinator, Gateway: gateway, Routes: route, Provider: provider}
	gateway.Writer = writer
	route.Writer = writer
	return gateway
}

func (p *recordingProvider) ReconcileGatewayGraph(ctx context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	p.graphSpecs = append(p.graphSpecs, spec)
	result := cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome(), Gateway: cloud.GatewayReadyResult(cloud.GatewayState{LoadBalancerID: "lb-1", VIPAddress: "192.0.2.10", ListenerID: "listener-1"})}
	if spec.Gateway.Disposition == cloud.GraphDelete {
		outcome, err := p.DeleteGateway(ctx, spec.Identity)
		result.Outcome = outcome
		result.Gateway.Outcome = outcome
		return result, err
	}
	for _, route := range spec.Routes {
		switch route.Disposition {
		case cloud.GraphDelete:
			outcome, err := p.DeleteRoute(ctx, route.Spec.Identity)
			result.Outcome = outcome
			result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: route.Spec.Identity, Result: cloud.RouteResult{Outcome: outcome}})
			return result, err
		case cloud.GraphEnsure:
			routeResult, err := p.EnsureRoute(ctx, route.Spec)
			result.Outcome = routeResult.Outcome
			result.Routes = append(result.Routes, cloud.RouteGraphResult{Identity: route.Spec.Identity, Result: routeResult})
			return result, err
		}
	}
	if spec.Gateway.Disposition == cloud.GraphEnsure {
		gatewayResult, err := p.EnsureGateway(ctx, spec.Gateway.Spec)
		result.Gateway = gatewayResult
		result.Outcome = gatewayResult.Outcome
		return result, err
	}
	return result, nil
}
