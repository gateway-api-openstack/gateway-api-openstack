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

package conformance

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestPreflightChecksIdentityDeclarationsAndIsolation(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    func(*Config, *gatewayv1.GatewayClass, *unstructured.Unstructured, *[]client.Object)
		wantError bool
	}{
		{name: "accepted profile"},
		{name: "wrong cluster", wantError: true, change: func(c *Config, _ *gatewayv1.GatewayClass, _ *unstructured.Unstructured, _ *[]client.Object) {
			c.ExpectedClusterUID = "other"
		}},
		{name: "foreign controller", wantError: true, change: func(_ *Config, gc *gatewayv1.GatewayClass, _ *unstructured.Unstructured, _ *[]client.Object) {
			gc.Spec.ControllerName = "foreign.test/controller"
		}},
		{name: "empty advertised support", wantError: true, change: func(_ *Config, gc *gatewayv1.GatewayClass, _ *unstructured.Unstructured, _ *[]client.Object) {
			gc.Status.SupportedFeatures = nil
		}},
		{name: "manual diagnostic preserves empty declaration", change: func(c *Config, gc *gatewayv1.GatewayClass, _ *unstructured.Unstructured, _ *[]client.Object) {
			c.Mode = diagnosticMode
			gc.Status.SupportedFeatures = nil
		}},
		{name: "NodePort class cannot use untouched fixtures", wantError: true, change: func(_ *Config, _ *gatewayv1.GatewayClass, parameters *unstructured.Unstructured, _ *[]client.Object) {
			parameters.Object["spec"] = map[string]any{"memberMode": "NodePort"}
		}},
		{name: "preexisting fixture", wantError: true, change: func(_ *Config, _ *gatewayv1.GatewayClass, _ *unstructured.Unstructured, objects *[]client.Object) {
			*objects = append(*objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gateway-conformance-infra"}})
		}},
		{name: "mixed bundle", wantError: true, change: func(_ *Config, _ *gatewayv1.GatewayClass, _ *unstructured.Unstructured, objects *[]client.Object) {
			(*objects)[1].(*apiextensionsv1.CustomResourceDefinition).Annotations["gateway.networking.k8s.io/bundle-version"] = "v1.5.0"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			gatewayClass := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: config.GatewayClassName, Generation: 1}, Spec: gatewayv1.GatewayClassSpec{
				ControllerName: gatewayv1.GatewayController(config.ControllerName),
				ParametersRef:  &gatewayv1.ParametersReference{Group: "gateway-api-openstack.github.io", Kind: "OpenStackGatewayClassConfig", Name: "test"},
			}, Status: gatewayv1.GatewayClassStatus{
				Conditions:        []metav1.Condition{{Type: string(gatewayv1.GatewayClassConditionStatusAccepted), Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Accepted", LastTransitionTime: metav1.Now()}},
				SupportedFeatures: []gatewayv1.SupportedFeature{{Name: "Gateway"}, {Name: "HTTPRoute"}, {Name: "ReferenceGrant"}},
			}}
			parameters := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "gateway-api-openstack.github.io/v1alpha1", "kind": "OpenStackGatewayClassConfig",
				"metadata": map[string]any{"name": "test"}, "spec": map[string]any{"memberMode": "Endpoint"},
			}}
			objects := []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(config.ExpectedClusterUID)}}}
			for _, name := range []string{"gateways", "gatewayclasses", "httproutes", "referencegrants"} {
				objects = append(objects, &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name + ".gateway.networking.k8s.io", Annotations: map[string]string{"gateway.networking.k8s.io/bundle-version": bundleVersion, "gateway.networking.k8s.io/channel": "standard"}}, Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: gatewayv1.GroupName}})
			}
			if test.change != nil {
				test.change(&config, gatewayClass, parameters, &objects)
			}
			objects = append(objects, gatewayClass, parameters)
			scheme := runtime.NewScheme()
			for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, gatewayv1.Install, apiextensionsv1.AddToScheme} {
				if err := install(scheme); err != nil {
					t.Fatal(err)
				}
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			fixtures := []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gateway-conformance-infra"}}}
			observed, err := preflight(t.Context(), kubeClient, config, fixtures)
			if (err != nil) != test.wantError {
				t.Fatalf("preflight error = %v", err)
			}
			if err == nil && len(observed.Status.SupportedFeatures) != len(gatewayClass.Status.SupportedFeatures) {
				t.Fatal("preflight changed advertised features")
			}
		})
	}
}
