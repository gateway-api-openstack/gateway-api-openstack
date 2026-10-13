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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
)

func TestParameterChangeWakesUnboundGatewaysWithoutClassStatusChange(t *testing.T) {
	cfg := testConfig()
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName, ParametersRef: &gatewayv1.ParametersReference{Group: configv1alpha1.GroupName, Kind: configv1alpha1.GatewayClassConfigKind, Name: "test-parameters"}}}
	unbound := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unbound"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(class.Name)}}
	bound := programmedTestGateway(cfg)
	unrelated := unbound.DeepCopy()
	unrelated.Name = "unrelated"
	unrelated.Spec.GatewayClassName = "other"
	foreignClass := class.DeepCopy()
	foreignClass.Name = "foreign"
	foreignClass.Spec.ControllerName = "foreign.example/controller"
	foreign := unbound.DeepCopy()
	foreign.Name = "foreign"
	foreign.Spec.GatewayClassName = "foreign"
	unsupportedClass := class.DeepCopy()
	unsupportedClass.Name = "unsupported"
	unsupportedClass.Spec.ParametersRef.Group = "other.example"
	unsupported := unbound.DeepCopy()
	unsupported.Name = "unsupported"
	unsupported.Spec.GatewayClassName = "unsupported"
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithObjects(class, unbound, bound, unrelated, foreignClass, foreign, unsupportedClass, unsupported).Build()
	reconciler := &Reconciler{Client: kubeClient, Config: cfg}
	// There is no GatewayClass status update in this scenario. The parameters
	// event itself must reach only Gateways that still need an initial binding.
	requests := reconciler.gatewayRequestsForParameters(context.Background(), testClassParameters())
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(unbound) {
		t.Fatalf("requests=%#v", requests)
	}
}
