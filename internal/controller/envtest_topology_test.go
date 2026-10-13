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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/gatewayclass"
)

func testGatewayClassTopologyIsolation(t *testing.T, apiClient, cacheClient client.Client, config controller.Config) {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "topology-isolation"}}
	if err := apiClient.Create(t.Context(), namespace); err != nil {
		t.Fatal(err)
	}

	parametersA := envtestClassParameters("isolation-a")
	parametersB := envtestClassParameters("isolation-b")
	parametersB.Spec.VIPSubnet.ID = "33333333-3333-3333-3333-333333333333"
	parametersB.Spec.MemberSubnet.ID = "44444444-4444-4444-4444-444444444444"
	parametersB.Spec.FloatingIP = configv1alpha1.FloatingIPConfig{
		Mode:            configv1alpha1.FloatingIPAllocate,
		ExternalNetwork: &configv1alpha1.ResourceReference{ID: "55555555-5555-5555-5555-555555555555"},
	}
	parametersB.Spec.FlavorID = "66666666-6666-6666-6666-666666666666"
	parametersB.Spec.AvailabilityZone = "zone-b"
	parametersB.Spec.NodeAddressType = string(corev1.NodeExternalIP)
	parametersB.Spec.HealthMonitor.Path = "/ready"

	classes := []struct {
		parameters *configv1alpha1.OpenStackGatewayClassConfig
		class      *gatewayv1.GatewayClass
		gateway    *gatewayv1.Gateway
		want       classconfig.Resolved
	}{
		{parameters: parametersA, want: classconfig.Resolved{
			Topology:        cloud.Topology{VIPSubnetID: "11111111-1111-1111-1111-111111111111", MemberSubnetID: "22222222-2222-2222-2222-222222222222"},
			NodeAddressType: corev1.NodeInternalIP,
			HealthMonitor:   cloud.HealthMonitorConfig{Path: "/", Delay: 10, Timeout: 5, MaxRetries: 3, MaxRetriesDown: 3},
		}},
		{parameters: parametersB, want: classconfig.Resolved{
			Topology: cloud.Topology{
				VIPSubnetID: "33333333-3333-3333-3333-333333333333", MemberSubnetID: "44444444-4444-4444-4444-444444444444",
				ExternalNetworkID: "55555555-5555-5555-5555-555555555555", FlavorID: "66666666-6666-6666-6666-666666666666", AvailabilityZone: "zone-b",
			},
			NodeAddressType: corev1.NodeExternalIP,
			HealthMonitor:   cloud.HealthMonitorConfig{Path: "/ready", Delay: 10, Timeout: 5, MaxRetries: 3, MaxRetriesDown: 3},
		}},
	}
	provider := &recordingProvider{}
	classReconciler := &gatewayclass.Reconciler{Client: cacheClient, APIReader: apiClient, Config: config, TopologyResolver: provider}
	reconciler := newEnvtestGatewayReconciler(cacheClient, apiClient, provider, config)

	reconcileClass := func(class *gatewayv1.GatewayClass, want metav1.ConditionStatus) {
		t.Helper()
		// Once Gateways exist, the reference finalizer is persisted before status.
		for range 2 {
			if _, err := classReconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(class)}); err != nil {
				t.Fatalf("reconcile GatewayClass %s: %v", class.Name, err)
			}
			if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(class), class); err != nil {
				t.Fatal(err)
			}
			waitForGatewayClassResourceVersion(t, cacheClient, class.Name, class.ResourceVersion)
		}
		accepted := meta.FindStatusCondition(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		if accepted == nil || accepted.Status != want || accepted.ObservedGeneration != class.Generation {
			t.Fatalf("GatewayClass %s Accepted = %#v, want %s for generation %d", class.Name, accepted, want, class.Generation)
		}
		if want == metav1.ConditionFalse && accepted.Reason != string(gatewayv1.GatewayClassReasonInvalidParameters) {
			t.Fatalf("GatewayClass %s rejection = %s, want InvalidParameters", class.Name, accepted.Reason)
		}
	}
	reconcileGateway := func(gateway *gatewayv1.Gateway) ctrl.Result {
		t.Helper()
		result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)})
		if err != nil {
			t.Fatalf("reconcile Gateway %s: %v", gateway.Name, err)
		}
		if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(gateway), gateway); err != nil {
			t.Fatal(err)
		}
		waitForGatewayResourceVersion(t, cacheClient, client.ObjectKeyFromObject(gateway), gateway.ResourceVersion, false)
		return result
	}

	for index := range classes {
		item := &classes[index]
		if err := apiClient.Create(t.Context(), item.parameters); err != nil {
			t.Fatal(err)
		}
		item.want.ParametersName, item.want.ParametersUID, item.want.ParametersGeneration = item.parameters.Name, item.parameters.UID, item.parameters.Generation
		item.class = &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: item.parameters.Name},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: config.ControllerName,
				ParametersRef:  &gatewayv1.ParametersReference{Group: configv1alpha1.GroupName, Kind: configv1alpha1.GatewayClassConfigKind, Name: item.parameters.Name},
			},
		}
		if err := apiClient.Create(t.Context(), item.class); err != nil {
			t.Fatal(err)
		}
		waitForGatewayClassResourceVersion(t, cacheClient, item.class.Name, item.class.ResourceVersion)
		reconcileClass(item.class, metav1.ConditionTrue)
		item.gateway = createEnvtestGateway(t, apiClient, cacheClient, namespace.Name, item.class.Name, item.class.Name)
		if result := reconcileGateway(item.gateway); !result.Requeue || len(provider.graphSpecs) != 0 {
			t.Fatalf("Gateway %s binding result = %#v, graph calls = %d, want a durable checkpoint before cloud calls", item.gateway.Name, result, len(provider.graphSpecs))
		}
	}

	assertIsolatedGraphs := func() {
		t.Helper()
		// Reverse creation order while sharing the same reconciler and provider.
		for index := len(classes) - 1; index >= 0; index-- {
			item := &classes[index]
			before := len(provider.graphSpecs)
			reconcileGateway(item.gateway)
			if len(provider.graphSpecs) != before+1 {
				t.Fatalf("Gateway %s issued %d graph calls, want one", item.gateway.Name, len(provider.graphSpecs)-before)
			}
			call := provider.graphSpecs[before]
			identity := controller.GatewayIdentity(config, item.gateway)
			wantTopology := item.want.Topology
			if call.Identity != identity || call.Gateway.Spec.Identity != identity || call.Gateway.Disposition != cloud.GraphEnsure || len(call.Routes) != 0 {
				t.Fatalf("Gateway %s received another graph's identity or components: %#v", item.gateway.Name, call)
			}
			got := call.Gateway.Spec
			if got.VIPSubnetID != wantTopology.VIPSubnetID || got.ExternalNetworkID != wantTopology.ExternalNetworkID || got.FlavorID != wantTopology.FlavorID || got.AvailabilityZone != wantTopology.AvailabilityZone {
				t.Fatalf("Gateway %s provider topology = %#v, want %#v", item.gateway.Name, got, wantTopology)
			}
			snapshot, err := controller.StoredGatewayTopology(config, item.gateway)
			if err != nil || snapshot != item.want {
				t.Fatalf("Gateway %s stored configuration = %#v, error = %v, want %#v", item.gateway.Name, snapshot, err, item.want)
			}
		}
	}
	assertIsolatedGraphs()

	// Both classes now select the other template. Neither bound Gateway may move.
	for index := range classes {
		item := &classes[index]
		item.class.Spec.ParametersRef.Name = classes[1-index].parameters.Name
		if err := apiClient.Update(t.Context(), item.class); err != nil {
			t.Fatal(err)
		}
		waitForGatewayClassResourceVersion(t, cacheClient, item.class.Name, item.class.ResourceVersion)
		reconcileClass(item.class, metav1.ConditionTrue)
	}
	assertIsolatedGraphs()

	for index := range classes {
		item := &classes[index]
		item.parameters.Spec.VIPSubnet.ID = "77777777-7777-7777-7777-777777777777"
		if err := apiClient.Update(t.Context(), item.parameters); err != nil {
			t.Fatal(err)
		}
	}
	for index := range classes {
		reconcileClass(classes[index].class, metav1.ConditionTrue)
	}
	assertIsolatedGraphs()

	for index := range classes {
		if err := apiClient.Delete(t.Context(), classes[index].parameters); err != nil {
			t.Fatal(err)
		}
	}
	for index := range classes {
		reconcileClass(classes[index].class, metav1.ConditionFalse)
	}
	// New reconciler dependencies must reconstruct the same isolated snapshots.
	reconciler = newEnvtestGatewayReconciler(cacheClient, apiClient, provider, config)
	assertIsolatedGraphs()
}
