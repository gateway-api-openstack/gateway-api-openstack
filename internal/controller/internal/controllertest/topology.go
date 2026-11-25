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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
)

// Synthetic subnet identities used by class and topology fixtures.
const (
	VIPSubnetID    = "11111111-1111-1111-1111-111111111111"
	memberSubnetID = "22222222-2222-2222-2222-222222222222"
)

// ClassParameters returns fresh NodePort class settings for ordinary tests.
func ClassParameters() *configv1alpha1.OpenStackGatewayClassConfig {
	return &configv1alpha1.OpenStackGatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-parameters", UID: "test-parameters-uid", Generation: 1},
		Spec: configv1alpha1.OpenStackGatewayClassConfigSpec{
			VIPSubnet:       configv1alpha1.ResourceReference{ID: VIPSubnetID},
			MemberSubnet:    configv1alpha1.ResourceReference{ID: memberSubnetID},
			NodeAddressType: string(corev1.NodeInternalIP),
			HealthMonitor:   configv1alpha1.HealthMonitorConfig{Path: "/"},
		},
	}
}

// BoundTopology returns the resolved topology matching ClassParameters.
func BoundTopology() classconfig.Resolved {
	return classconfig.Resolved{
		Topology:        cloud.Topology{MemberMode: cloud.MemberModeNodePort, VIPSubnetID: VIPSubnetID, MemberSubnetID: memberSubnetID},
		NodeAddressType: corev1.NodeInternalIP,
		HealthMonitor:   cloud.HealthMonitorConfig{Path: "/", Delay: 10, Timeout: 5, MaxRetries: 3, MaxRetriesDown: 3},
		ParametersName:  "test-parameters", ParametersUID: "test-parameters-uid", ParametersGeneration: 1,
	}
}

// CompleteGatewayTopology completes an identity-bound fixture without replacing an existing topology binding.
func CompleteGatewayTopology(cfg controller.Config, gateway *gatewayv1.Gateway) {
	if controller.HasGatewayTopologyBinding(cfg, gateway) {
		return
	}
	for _, key := range []string{cfg.GatewayClusterIDAnnotation(), cfg.GatewayProjectIDAnnotation()} {
		if gateway.Annotations[key] == "" {
			return
		}
	}
	if err := controller.BindGatewayTopology(cfg, gateway, BoundTopology()); err != nil {
		panic(err)
	}
}
