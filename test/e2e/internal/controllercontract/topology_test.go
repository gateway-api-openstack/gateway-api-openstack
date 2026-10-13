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

package controllercontract

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
)

func TestTopologySettingsUsesOnlyApprovedInfrastructure(t *testing.T) {
	for _, network := range []string{"", "external-network"} {
		settings := TopologySettings{VIPSubnetID: "vip-subnet", MemberSubnetID: "member-subnet", ExternalNetworkID: network}
		parameters := settings.ClassConfig("run-config", "run-id")
		if parameters.Name != "run-config" || parameters.Namespace != "" || parameters.Annotations[RunAnnotation] != "run-id" ||
			parameters.Spec.VIPSubnet.ID != settings.VIPSubnetID || parameters.Spec.MemberSubnet.ID != settings.MemberSubnetID {
			t.Fatalf("class configuration escaped approved identity or topology: %#v", parameters)
		}
		if network == "" && (parameters.Spec.FloatingIP.Mode != configv1alpha1.FloatingIPNone || parameters.Spec.FloatingIP.ExternalNetwork != nil) {
			t.Fatal("VIP-only run allocated an address")
		}
		if network != "" && (parameters.Spec.FloatingIP.Mode != configv1alpha1.FloatingIPAllocate || parameters.Spec.FloatingIP.ExternalNetwork.ID != network) {
			t.Fatal("Floating IP run changed the approved network")
		}
	}
}

func TestRequireClassConfigCRDRejectsUnavailableOrDifferentAPI(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	valid := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: ClassConfigCRDName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: configv1alpha1.GroupName, Scope: apiextensionsv1.ClusterScoped,
			Names:    apiextensionsv1.CustomResourceDefinitionNames{Kind: configv1alpha1.GatewayClassConfigKind, Plural: "openstackgatewayclassconfigs"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: configv1alpha1.GroupVersion.Version, Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"}}}},
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}},
	}
	for _, test := range []struct {
		name      string
		change    func(*apiextensionsv1.CustomResourceDefinition)
		wantError bool
	}{
		{name: "established version"},
		{name: "unserved version", change: func(crd *apiextensionsv1.CustomResourceDefinition) { crd.Spec.Versions[0].Served = false }, wantError: true},
		{name: "unestablished", change: func(crd *apiextensionsv1.CustomResourceDefinition) { crd.Status.Conditions = nil }, wantError: true},
		{name: "wrong scope", change: func(crd *apiextensionsv1.CustomResourceDefinition) { crd.Spec.Scope = apiextensionsv1.NamespaceScoped }, wantError: true},
		{name: "missing schema", change: func(crd *apiextensionsv1.CustomResourceDefinition) { crd.Spec.Versions[0].Schema = nil }, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := valid.DeepCopy()
			if test.change != nil {
				test.change(definition)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(definition).Build()
			if err := RequireClassConfigCRD(context.Background(), reader); (err != nil) != test.wantError {
				t.Fatalf("RequireClassConfigCRD() = %v", err)
			}
		})
	}
	if err := RequireClassConfigCRD(context.Background(), fake.NewClientBuilder().WithScheme(scheme).Build()); err == nil {
		t.Fatal("missing CRD was accepted")
	}
}
