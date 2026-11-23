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
	"reflect"
	"testing"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

func TestClientBuildersKeepExplicitFixturesIndependent(t *testing.T) {
	cfg := controller.Config{ControllerName: "example.test/gateway", OpenStackProjectID: "project", ClusterID: "cluster"}
	parameters := ClassParameters()
	parameters.Spec.VIPSubnet.ID = "custom-subnet"
	definition := gatewayAPICRD("httproutes.gateway.networking.k8s.io", "unsupported")
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack"},
		Spec:       gatewayv1.GatewayClassSpec{ParametersRef: &gatewayv1.ParametersReference{Group: "example.test", Kind: "Invalid", Name: "explicit"}},
		Status:     gatewayv1.GatewayClassStatus{Conditions: []metav1.Condition{{Type: string(gatewayv1.GatewayClassConditionStatusSupportedVersion), Status: metav1.ConditionFalse, Reason: "Unsupported", LastTransitionTime: metav1.Now()}}},
	}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default", Annotations: map[string]string{
		cfg.GatewayClusterIDAnnotation(): cfg.ClusterID, cfg.GatewayProjectIDAnnotation(): cfg.OpenStackProjectID,
		cfg.GatewayTopologyAnnotation(): "invalid-topology",
	}}}
	expectedClass, expectedGateway := class.DeepCopy(), gateway.DeepCopy()
	builder := NewClientBuilder(Scheme(t), cfg).WithObjects(parameters, definition, class, gateway)
	if !reflect.DeepEqual(class, expectedClass) || !reflect.DeepEqual(gateway, expectedGateway) {
		t.Fatal("builder repaired an explicitly invalid fixture")
	}
	explicit := builder.Build()
	var definitions apiextensionsv1.CustomResourceDefinitionList
	if err := explicit.List(t.Context(), &definitions); err != nil {
		t.Fatal(err)
	}
	if len(definitions.Items) != 1 || definitions.Items[0].Annotations[gatewayconsts.BundleVersionAnnotation] != "unsupported" {
		t.Fatalf("builder replaced an incomplete bundle: %#v", definitions.Items)
	}
	var stored configv1alpha1.OpenStackGatewayClassConfig
	if err := explicit.Get(t.Context(), client.ObjectKeyFromObject(parameters), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.VIPSubnet.ID != "custom-subnet" {
		t.Fatal("builder replaced explicit class parameters")
	}

	independentClass := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}}
	independentGateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default", Annotations: map[string]string{
		cfg.GatewayClusterIDAnnotation(): cfg.ClusterID, cfg.GatewayProjectIDAnnotation(): cfg.OpenStackProjectID,
	}}}
	independent := NewClientBuilder(Scheme(t), cfg).WithObjects(independentClass, independentGateway).Build()
	if err := independent.Get(t.Context(), client.ObjectKeyFromObject(parameters), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.VIPSubnet.ID != VIPSubnetID {
		t.Fatal("independent builder inherited another fixture's parameters")
	}
	definitions.Items = nil
	if err := independent.List(t.Context(), &definitions); err != nil {
		t.Fatal(err)
	}
	if len(definitions.Items) != 4 {
		t.Fatalf("independent builder inherited the incomplete bundle: %d definitions", len(definitions.Items))
	}
	for _, definition := range definitions.Items {
		if definition.Annotations[gatewayconsts.BundleVersionAnnotation] != gatewayconsts.BundleVersion {
			t.Fatal("independent builder inherited another fixture's version")
		}
	}
	if !meta.IsStatusConditionTrue(independentClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion)) {
		t.Fatal("builder did not complete the caller's class fixture")
	}
	if _, err := controller.StoredGatewayTopology(cfg, independentGateway); err != nil {
		t.Fatalf("builder did not complete the caller's Gateway fixture: %v", err)
	}
}

func TestRawClientBuilderRegistersIndexesWithoutDefaults(t *testing.T) {
	cfg := controller.Config{ControllerName: "example.test/gateway"}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "openstack"}}
	kubeClient := RawClientBuilder(Scheme(t), cfg).WithObjects(gateway).Build()
	var gateways gatewayv1.GatewayList
	if err := kubeClient.List(t.Context(), &gateways, client.MatchingFields{controller.IndexGatewayByClass: "openstack"}); err != nil {
		t.Fatal(err)
	}
	if len(gateways.Items) != 1 || len(gateways.Items[0].Annotations) != 0 || len(gateways.Items[0].Finalizers) != 0 {
		t.Fatalf("raw builder changed the Gateway fixture or omitted its index: %#v", gateways.Items)
	}
	for _, list := range []client.ObjectList{&apiextensionsv1.CustomResourceDefinitionList{}, &configv1alpha1.OpenStackGatewayClassConfigList{}} {
		if err := kubeClient.List(t.Context(), list); err != nil {
			t.Fatal(err)
		}
		if meta.LenList(list) != 0 {
			t.Fatalf("raw builder added default objects: %#v", list)
		}
	}
}
