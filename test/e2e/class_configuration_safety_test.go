//go:build e2e

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

package e2e

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/controllercontract"
)

func TestLocalLifecycleClassAllowlistRejectsIdentityAndReferenceChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*gatewayv1.GatewayClass)
	}{
		{name: "UID", change: func(class *gatewayv1.GatewayClass) { class.UID = "replacement-uid" }},
		{name: "run annotation", change: func(class *gatewayv1.GatewayClass) { class.Annotations[runAnnotation] = "another-run" }},
		{name: "deletion", change: func(class *gatewayv1.GatewayClass) {
			now := metav1.Now()
			class.DeletionTimestamp, class.Finalizers = &now, []string{"test"}
		}},
		{name: "controller", change: func(class *gatewayv1.GatewayClass) { class.Spec.ControllerName = "foreign.example.test/controller" }},
		{name: "parameters name", change: func(class *gatewayv1.GatewayClass) { class.Spec.ParametersRef.Name = "unapproved-parameters" }},
		{name: "parameters namespace", change: func(class *gatewayv1.GatewayClass) {
			namespace := gatewayv1.Namespace("unapproved")
			class.Spec.ParametersRef.Namespace = &namespace
		}},
	} {
		for _, second := range []bool{false, true} {
			suffix := "/primary"
			if second {
				suffix = "/second"
			}
			t.Run(test.name+suffix, func(t *testing.T) {
				suite, objects := localLifecycleClassFixture(t)
				if err := suite.verifyRunScopedControllerClasses(context.Background()); err != nil {
					t.Fatalf("approved classes were rejected: %v", err)
				}
				index := 0
				if second {
					index = 1
				}
				test.change(objects[index].(*gatewayv1.GatewayClass))
				suite.client = fake.NewClientBuilder().WithScheme(runScopedControllerScheme(t)).WithObjects(objects...).Build()
				if err := suite.verifyRunScopedControllerClasses(context.Background()); err == nil {
					t.Fatal("class validation accepted an unapproved identity or reference")
				}
			})
		}
	}
}

func TestLocalLifecycleClassAllowlistRejectsMissingClasses(t *testing.T) {
	for _, index := range []int{0, 1} {
		suite, objects := localLifecycleClassFixture(t)
		objects = append(objects[:index], objects[index+1:]...)
		suite.client = fake.NewClientBuilder().WithScheme(runScopedControllerScheme(t)).WithObjects(objects...).Build()
		if err := suite.verifyRunScopedControllerClasses(context.Background()); err == nil {
			t.Fatal("class validation accepted a missing run-scoped GatewayClass")
		}
	}
}

func TestLocalLifecycleClassAllowlistAcceptsOnlyRecordedReferenceSwap(t *testing.T) {
	suite, objects := localLifecycleClassFixture(t)
	objects[0].(*gatewayv1.GatewayClass).Spec.ParametersRef = suite.secondClass.Spec.ParametersRef.DeepCopy()
	suite.client = fake.NewClientBuilder().WithScheme(runScopedControllerScheme(t)).WithObjects(objects...).Build()
	if err := suite.verifyRunScopedControllerClasses(context.Background()); err == nil {
		t.Fatal("an unrecorded parameter reference change was accepted")
	}
	suite.classParametersName = suite.secondClassConfig.Name
	if err := suite.verifyRunScopedControllerClasses(context.Background()); err != nil {
		t.Fatalf("the recorded reference swap was rejected: %v", err)
	}
	objects[3].(*configv1alpha1.OpenStackGatewayClassConfig).Spec.VIPSubnet.ID = "unapproved-subnet"
	suite.client = fake.NewClientBuilder().WithScheme(runScopedControllerScheme(t)).WithObjects(objects...).Build()
	if err := suite.verifyRunScopedControllerClasses(context.Background()); err == nil {
		t.Fatal("a reference swap accepted changed target parameters")
	}
}

func TestLocalLifecycleParametersRetainIdentityAndExactSpec(t *testing.T) {
	for _, index := range []int{2, 3} {
		for _, change := range []func(*configv1alpha1.OpenStackGatewayClassConfig){
			func(parameters *configv1alpha1.OpenStackGatewayClassConfig) { parameters.UID = "replacement-uid" },
			func(parameters *configv1alpha1.OpenStackGatewayClassConfig) {
				parameters.Spec.MemberSubnet.ID = "unapproved-subnet"
			},
			func(parameters *configv1alpha1.OpenStackGatewayClassConfig) { *parameters.Spec.HealthMonitor.Delay++ },
		} {
			suite, objects := localLifecycleClassFixture(t)
			change(objects[index].(*configv1alpha1.OpenStackGatewayClassConfig))
			suite.client = fake.NewClientBuilder().WithScheme(runScopedControllerScheme(t)).WithObjects(objects...).Build()
			if err := suite.verifyRunScopedControllerClasses(context.Background()); err == nil {
				t.Fatal("class parameters validation accepted an unapproved UID or spec")
			}
		}
	}
}

func TestSecondClassCleanupRetainsForeignObjects(t *testing.T) {
	for _, index := range []int{1, 3} {
		for _, change := range []func(client.Object){
			func(object client.Object) { object.SetUID("replacement-uid") },
			func(object client.Object) { object.GetAnnotations()[runAnnotation] = "another-run" },
		} {
			suite, objects := localLifecycleClassFixture(t)
			foreign := objects[index]
			change(foreign)
			suite.client = fake.NewClientBuilder().WithScheme(runScopedControllerScheme(t)).WithObjects(objects...).Build()
			if err := suite.deleteSecondClass(context.Background()); err == nil {
				t.Fatal("cleanup accepted a foreign second class object")
			}
			if err := suite.client.Get(context.Background(), client.ObjectKeyFromObject(foreign), foreign.DeepCopyObject().(client.Object)); err != nil {
				t.Fatalf("foreign class object was not retained: %v", err)
			}
		}
	}
}

func TestSecondClassCleanupRetainsUncertainCreate(t *testing.T) {
	suite, _ := localLifecycleClassFixture(t)
	suite.secondClass.UID = ""
	if err := suite.deleteSecondClass(context.Background()); err == nil {
		t.Fatal("cleanup accepted an uncertain class creation")
	}
	if err := suite.client.Get(context.Background(), client.ObjectKeyFromObject(suite.secondClass), &gatewayv1.GatewayClass{}); err != nil {
		t.Fatalf("class with an uncertain creation outcome was removed: %v", err)
	}
}

func TestTopologyMatchesParametersChecksEveryFrozenSetting(t *testing.T) {
	suite, _ := localLifecycleClassFixture(t)
	parameters := suite.classConfig
	resolved := classconfig.Resolved{
		ParametersName: parameters.Name, ParametersUID: parameters.UID, ParametersGeneration: parameters.Generation,
		NodeAddressType: "InternalIP",
		Topology:        cloud.Topology{VIPSubnetID: parameters.Spec.VIPSubnet.ID, MemberSubnetID: parameters.Spec.MemberSubnet.ID},
		HealthMonitor:   cloud.HealthMonitorConfig{Path: "/", Delay: 10, Timeout: 5, MaxRetries: 3, MaxRetriesDown: 3},
	}
	if err := topologyMatchesParameters(resolved, parameters); err != nil {
		t.Fatalf("matching bound settings were rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*classconfig.Resolved)
	}{
		{name: "parameter name", change: func(value *classconfig.Resolved) { value.ParametersName = "other-parameters" }},
		{name: "parameter UID", change: func(value *classconfig.Resolved) { value.ParametersUID = "other-uid" }},
		{name: "parameter generation", change: func(value *classconfig.Resolved) { value.ParametersGeneration++ }},
		{name: "VIP subnet", change: func(value *classconfig.Resolved) { value.Topology.VIPSubnetID = "other-subnet" }},
		{name: "member subnet", change: func(value *classconfig.Resolved) { value.Topology.MemberSubnetID = "other-subnet" }},
		{name: "external network", change: func(value *classconfig.Resolved) { value.Topology.ExternalNetworkID = "unapproved-network" }},
		{name: "flavor", change: func(value *classconfig.Resolved) { value.Topology.FlavorID = "unapproved-flavor" }},
		{name: "availability zone", change: func(value *classconfig.Resolved) { value.Topology.AvailabilityZone = "unapproved-zone" }},
		{name: "address type", change: func(value *classconfig.Resolved) { value.NodeAddressType = "ExternalIP" }},
		{name: "monitor path", change: func(value *classconfig.Resolved) { value.HealthMonitor.Path = "/other" }},
		{name: "monitor delay", change: func(value *classconfig.Resolved) { value.HealthMonitor.Delay++ }},
		{name: "monitor timeout", change: func(value *classconfig.Resolved) { value.HealthMonitor.Timeout++ }},
		{name: "monitor retries", change: func(value *classconfig.Resolved) { value.HealthMonitor.MaxRetries++ }},
		{name: "monitor down retries", change: func(value *classconfig.Resolved) { value.HealthMonitor.MaxRetriesDown++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := resolved
			test.change(&changed)
			if err := topologyMatchesParameters(changed, parameters); err == nil {
				t.Fatal("a Gateway with different bound settings was accepted")
			}
		})
	}
}

func localLifecycleClassFixture(t *testing.T) (*openStackSuite, []client.Object) {
	t.Helper()
	config := e2eConfig{RunID: "run-1234", GatewayClassName: "gao-e2e-run-1234", ControllerName: "e2e.example.test/gao-e2e-run-1234", PollInterval: time.Millisecond}
	var classes []*gatewayv1.GatewayClass
	var parameters []*configv1alpha1.OpenStackGatewayClassConfig
	for _, suffix := range []string{"", "-second"} {
		name := config.GatewayClassName + suffix
		parameter := (controllercontract.TopologySettings{
			VIPSubnetID:    "11111111-1111-4111-8111-111111111111",
			MemberSubnetID: "22222222-2222-4222-8222-222222222222",
		}).ClassConfig(name, config.RunID)
		parameter.UID, parameter.Generation = types.UID(name+"-parameters-uid"), 1
		parameters = append(parameters, parameter)
		classes = append(classes, &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-class-uid"), Annotations: map[string]string{runAnnotation: config.RunID}},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: gatewayv1.GatewayController(config.ControllerName),
				ParametersRef:  &gatewayv1.ParametersReference{Group: gatewayv1.Group(configv1alpha1.GroupName), Kind: gatewayv1.Kind(configv1alpha1.GatewayClassConfigKind), Name: name},
			},
		})
	}
	objects := []client.Object{classes[0], classes[1], parameters[0], parameters[1]}
	return &openStackSuite{
		config:       config,
		client:       fake.NewClientBuilder().WithScheme(runScopedControllerScheme(t)).WithObjects(objects...).Build(),
		createdClass: true, gatewayClassName: classes[0].Name, gatewayClassUID: classes[0].UID,
		classConfig: parameters[0].DeepCopy(), secondClassConfig: parameters[1].DeepCopy(), secondClass: classes[1].DeepCopy(),
	}, objects
}
