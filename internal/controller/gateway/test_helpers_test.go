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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
)

type Config = controller.Config

const (
	testVIPSubnetID    = "11111111-1111-1111-1111-111111111111"
	testMemberSubnetID = "22222222-2222-2222-2222-222222222222"
)

const (
	providerFailureRequeue   = 2 * time.Minute
	ownershipConflictRequeue = 10 * time.Minute
)

var (
	Condition    = controller.Condition
	SetCondition = controller.SetCondition
)

var requiredGatewayAPICRDs = []string{
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
}

type recordingProvider struct {
	gatewaySpecs     []cloud.GatewaySpec
	gatewayResult    *cloud.GatewayResult
	gatewayErr       error
	gatewayDeleteOut *cloud.Outcome
	gatewayDeleteErr error
	routeSpecs       []cloud.RouteSpec
	routeResult      *cloud.RouteResult
	routeErr         error
	routeDeleteOut   *cloud.Outcome
	routeDeleteErr   error
	deletedGateways  []cloud.Identity
	deletedRoutes    []cloud.Identity
}

func (p *recordingProvider) EnsureGateway(_ context.Context, spec cloud.GatewaySpec) (cloud.GatewayResult, error) {
	p.gatewaySpecs = append(p.gatewaySpecs, spec)
	if p.gatewayErr != nil {
		return cloud.GatewayResult{}, p.gatewayErr
	}
	if p.gatewayResult != nil {
		return *p.gatewayResult, nil
	}
	return cloud.GatewayReadyResult(cloud.GatewayState{LoadBalancerID: "lb-1", VIPAddress: "192.0.2.10", ListenerID: "listener-1"}), nil
}

func (p *recordingProvider) DeleteGateway(_ context.Context, identity cloud.Identity) (cloud.Outcome, error) {
	p.deletedGateways = append(p.deletedGateways, identity)
	if p.gatewayDeleteErr != nil {
		return cloud.Outcome{}, p.gatewayDeleteErr
	}
	if p.gatewayDeleteOut != nil {
		return *p.gatewayDeleteOut, nil
	}
	return cloud.ReadyOutcome(), nil
}

func (p *recordingProvider) EnsureRoute(_ context.Context, spec cloud.RouteSpec) (cloud.RouteResult, error) {
	p.routeSpecs = append(p.routeSpecs, spec)
	if p.routeErr != nil {
		return cloud.RouteResult{}, p.routeErr
	}
	if p.routeResult != nil {
		return *p.routeResult, nil
	}
	return cloud.RouteReadyResult(cloud.RouteState{PoolID: "pool-1"}), nil
}

func (p *recordingProvider) DeleteRoute(_ context.Context, identity cloud.Identity) (cloud.Outcome, error) {
	p.deletedRoutes = append(p.deletedRoutes, identity)
	if p.routeDeleteErr != nil {
		return cloud.Outcome{}, p.routeDeleteErr
	}
	if p.routeDeleteOut != nil {
		return *p.routeDeleteOut, nil
	}
	return cloud.ReadyOutcome(), nil
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

func gatewayBindingAnnotations(cfg Config, listenerPort string) map[string]string {
	return map[string]string{
		cfg.GatewayListenerPortAnnotation(): listenerPort,
		cfg.GatewayClusterIDAnnotation():    cfg.ClusterID,
		cfg.GatewayProjectIDAnnotation():    cfg.OpenStackProjectID,
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

type controllerTestClientBuilder struct {
	builder              *fake.ClientBuilder
	hasGatewayAPICRDs    bool
	config               Config
	hasDefaultParameters bool
}

// WithObjects gives ordinary controller tests the supported bundle that the
// version controller establishes before provisioning can begin. Version tests
// pass explicit CRDs or conditions to override this fixture.
func (b *controllerTestClientBuilder) WithObjects(objects ...client.Object) *controllerTestClientBuilder {
	for _, object := range objects {
		switch value := object.(type) {
		case *configv1alpha1.OpenStackGatewayClassConfig:
			if value.Name == "test-parameters" {
				b.hasDefaultParameters = true
			}
		case *gatewayv1.GatewayClass:
			if value.Spec.ParametersRef == nil {
				value.Spec.ParametersRef = &gatewayv1.ParametersReference{Group: configv1alpha1.GroupName, Kind: configv1alpha1.GatewayClassConfigKind, Name: "test-parameters"}
			}
		case *gatewayv1.Gateway:
			completeGatewayTopologyFixture(b.config, value)
		case *gatewayv1.HTTPRoute:
			completeRouteLifecycleFixture(b.config, value)
		}
		if definition, ok := object.(*apiextensionsv1.CustomResourceDefinition); ok &&
			(definition.Spec.Group == gatewayv1.GroupName || definition.Spec.Group == "gateway.networking.x-k8s.io") {
			b.hasGatewayAPICRDs = true
		}
		gatewayClass, ok := object.(*gatewayv1.GatewayClass)
		if !ok || meta.FindStatusCondition(
			gatewayClass.Status.Conditions,
			string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		) != nil {
			continue
		}
		SetCondition(&gatewayClass.Status.Conditions, Condition(
			string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
			metav1.ConditionTrue,
			string(gatewayv1.GatewayClassReasonSupportedVersion),
			"Installed Gateway API CRDs use supported bundle version "+gatewayconsts.BundleVersion,
			gatewayClass.Generation,
		))
	}
	b.builder = b.builder.WithObjects(objects...)
	return b
}

func (b *controllerTestClientBuilder) WithStatusSubresource(objects ...client.Object) *controllerTestClientBuilder {
	b.builder = b.builder.WithStatusSubresource(objects...)
	return b
}

func (b *controllerTestClientBuilder) Build() client.WithWatch {
	if !b.hasDefaultParameters {
		b.builder = b.builder.WithObjects(testClassParameters())
	}
	if !b.hasGatewayAPICRDs {
		definitions := make([]client.Object, 0, len(requiredGatewayAPICRDs))
		for _, name := range requiredGatewayAPICRDs {
			definitions = append(definitions, &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
					Annotations: map[string]string{
						gatewayconsts.BundleVersionAnnotation: gatewayconsts.BundleVersion,
					},
				},
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: gatewayv1.GroupName},
			})
		}
		b.builder = b.builder.WithObjects(definitions...)
	}
	return b.builder.Build()
}

func indexedFakeClientBuilder(scheme *runtime.Scheme, config Config) *controllerTestClientBuilder {
	return &controllerTestClientBuilder{builder: rawIndexedFakeClientBuilder(scheme, config), config: config}
}

func rawIndexedFakeClientBuilder(scheme *runtime.Scheme, config Config) *fake.ClientBuilder {
	builder := fake.NewClientBuilder().WithScheme(scheme)
	indexer := &fakeFieldIndexer{builder: builder}
	if err := controller.SetupIndexes(context.Background(), indexer, config); err != nil {
		panic(err)
	}
	return builder
}

func supportedGatewayAPICRDs() []client.Object {
	definitions := make([]client.Object, 0, len(requiredGatewayAPICRDs))
	for _, name := range requiredGatewayAPICRDs {
		definitions = append(definitions, gatewayAPICRD(name, gatewayconsts.BundleVersion))
	}
	return definitions
}

func gatewayAPICRD(name, bundleVersion string) *apiextensionsv1.CustomResourceDefinition {
	annotations := map[string]string{}
	if bundleVersion != "" {
		annotations[gatewayconsts.BundleVersionAnnotation] = bundleVersion
	}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Group: gatewayv1.GroupName},
	}
}

type fakeFieldIndexer struct {
	builder *fake.ClientBuilder
}

func (i *fakeFieldIndexer) IndexField(
	_ context.Context,
	object client.Object,
	field string,
	extract client.IndexerFunc,
) error {
	i.builder.WithIndex(object, field, extract)
	return nil
}

// These helpers construct ordinary Phase 3 fixtures. Tests for incomplete or
// corrupted bindings use rawIndexedFakeClientBuilder and explicit metadata.
func testClassParameters() *configv1alpha1.OpenStackGatewayClassConfig {
	return &configv1alpha1.OpenStackGatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-parameters", UID: "test-parameters-uid", Generation: 1},
		Spec: configv1alpha1.OpenStackGatewayClassConfigSpec{
			VIPSubnet:       configv1alpha1.ResourceReference{ID: testVIPSubnetID},
			MemberSubnet:    configv1alpha1.ResourceReference{ID: testMemberSubnetID},
			NodeAddressType: string(corev1.NodeInternalIP),
			HealthMonitor:   configv1alpha1.HealthMonitorConfig{Path: "/"},
		},
	}
}

func testBoundTopology() classconfig.Resolved {
	return classconfig.Resolved{
		Topology:        cloud.Topology{VIPSubnetID: testVIPSubnetID, MemberSubnetID: testMemberSubnetID},
		NodeAddressType: corev1.NodeInternalIP,
		HealthMonitor:   cloud.HealthMonitorConfig{Path: "/", Delay: 10, Timeout: 5, MaxRetries: 3, MaxRetriesDown: 3},
		ParametersName:  "test-parameters", ParametersUID: "test-parameters-uid", ParametersGeneration: 1,
	}
}

func completeGatewayTopologyFixture(cfg Config, gateway *gatewayv1.Gateway) {
	if controller.HasGatewayTopologyBinding(cfg, gateway) {
		return
	}
	for _, key := range []string{cfg.GatewayListenerPortAnnotation(), cfg.GatewayClusterIDAnnotation(), cfg.GatewayProjectIDAnnotation()} {
		if gateway.Annotations[key] == "" {
			return
		}
	}
	if err := controller.BindGatewayTopology(cfg, gateway, testBoundTopology()); err != nil {
		panic(err)
	}
}

func completeRouteLifecycleFixture(cfg Config, route *gatewayv1.HTTPRoute) {
	if _, present := route.Annotations[cfg.RouteLifecycleAnnotation()]; present {
		return
	}
	for _, key := range []string{cfg.RouteGatewayNamespaceAnnotation(), cfg.RouteGatewayNameAnnotation(), cfg.RouteGatewayUIDAnnotation(), cfg.RouteClusterIDAnnotation(), cfg.RouteProjectIDAnnotation()} {
		if route.Annotations[key] == "" {
			return
		}
	}
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, cfg.RouteBindingFinalizerPrefix()) {
			route.Annotations[cfg.RouteLifecycleAnnotation()] = controller.RouteAttached
			return
		}
	}
}

func (*recordingProvider) ResolveTopology(_ context.Context, request cloud.TopologyRequest) (cloud.Topology, error) {
	topology := cloud.Topology{VIPSubnetID: request.VIPSubnet.ID, MemberSubnetID: request.MemberSubnet.ID, FlavorID: request.FlavorID, AvailabilityZone: request.AvailabilityZone}
	if request.ExternalNetwork != nil {
		topology.ExternalNetworkID = request.ExternalNetwork.ID
	}
	return topology, nil
}
