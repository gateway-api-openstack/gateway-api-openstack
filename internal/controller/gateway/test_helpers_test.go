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
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
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

func (p *recordingProvider) setGatewayResponse(result cloud.GatewayResult, err error) {
	p.Respond = func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
		return cloud.GatewayGraphResult{Outcome: result.Outcome, Gateway: result}, err
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

func gatewayBindingAnnotations(cfg Config) map[string]string {
	return map[string]string{

		cfg.GatewayClusterIDAnnotation(): cfg.ClusterID,
		cfg.GatewayProjectIDAnnotation(): cfg.OpenStackProjectID,
	}
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

func testClassParameters() *configv1alpha1.OpenStackGatewayClassConfig {
	return controllertest.ClassParameters()
}

func testBoundTopology() classconfig.Resolved {
	return controllertest.BoundTopology()
}

func (*recordingProvider) ResolveTopology(_ context.Context, request cloud.TopologyRequest) (cloud.Topology, error) {
	topology := cloud.Topology{VIPSubnetID: request.VIPSubnet.ID, MemberSubnetID: request.MemberSubnet.ID, FlavorID: request.FlavorID, AvailabilityZone: request.AvailabilityZone}
	if request.ExternalNetwork != nil {
		topology.ExternalNetworkID = request.ExternalNetwork.ID
	}
	return topology, nil
}
