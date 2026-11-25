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

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	gatewaycontroller "github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/gateway"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
	httproutecontroller "github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/httproute"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/internal/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type recordingProvider struct {
	controllertest.GraphProvider
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
	return controllertest.Scheme(t)
}

func (*recordingProvider) ResolveTopology(_ context.Context, request cloud.TopologyRequest) (cloud.Topology, error) {
	topology := cloud.Topology{VIPSubnetID: request.VIPSubnet.ID, MemberSubnetID: request.MemberSubnet.ID, FlavorID: request.FlavorID, AvailabilityZone: request.AvailabilityZone}
	if request.ExternalNetwork != nil {
		topology.ExternalNetworkID = request.ExternalNetwork.ID
	}
	for _, reference := range request.VIPSecurityGroups {
		topology.VIPSecurityGroupIDs = append(topology.VIPSecurityGroupIDs, reference.ID)
	}
	for _, reference := range request.BackendSecurityGroups {
		topology.BackendSecurityGroupIDs = append(topology.BackendSecurityGroupIDs, reference.ID)
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
