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
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
)

// ClassConfigCRDName identifies the operator-installed configuration CRD.
const ClassConfigCRDName = "openstackgatewayclassconfigs." + configv1alpha1.GroupName

// TopologySettings contains existing infrastructure approved for one E2E run.
type TopologySettings struct {
	VIPSubnetID       string
	MemberSubnetID    string
	ExternalNetworkID string
}

// ClassConfig builds the run-scoped class parameters without cloud lookups.
func (s TopologySettings) ClassConfig(name, runID string) *configv1alpha1.OpenStackGatewayClassConfig {
	floatingIP := configv1alpha1.FloatingIPConfig{Mode: configv1alpha1.FloatingIPNone}
	if s.ExternalNetworkID != "" {
		floatingIP.Mode = configv1alpha1.FloatingIPAllocate
		floatingIP.ExternalNetwork = &configv1alpha1.ResourceReference{ID: s.ExternalNetworkID}
	}
	return &configv1alpha1.OpenStackGatewayClassConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: configv1alpha1.GroupVersion.String(), Kind: configv1alpha1.GatewayClassConfigKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{RunAnnotation: runID}},
		Spec: configv1alpha1.OpenStackGatewayClassConfigSpec{
			VIPSubnet: configv1alpha1.ResourceReference{ID: s.VIPSubnetID}, MemberSubnet: configv1alpha1.ResourceReference{ID: s.MemberSubnetID},
			FloatingIP: floatingIP, NodeAddressType: "InternalIP",
			HealthMonitor: configv1alpha1.HealthMonitorConfig{Path: "/", Delay: int32Pointer(10), Timeout: int32Pointer(5), MaxRetries: int32Pointer(3), MaxRetriesDown: int32Pointer(3)},
		},
	}
}

// RequireClassConfigCRD verifies the installation prerequisite without changing
// a shared cluster API or adopting an existing CRD into test ownership.
func RequireClassConfigCRD(ctx context.Context, reader client.Reader) error {
	var definition apiextensionsv1.CustomResourceDefinition
	if err := reader.Get(ctx, client.ObjectKey{Name: ClassConfigCRDName}, &definition); err != nil {
		return fmt.Errorf("read the operator-installed class configuration CRD: %w", err)
	}
	if definition.Spec.Group != configv1alpha1.GroupName || definition.Spec.Scope != apiextensionsv1.ClusterScoped ||
		definition.Spec.Names.Kind != configv1alpha1.GatewayClassConfigKind || definition.Spec.Names.Plural != "openstackgatewayclassconfigs" || !definition.DeletionTimestamp.IsZero() {
		return fmt.Errorf("the installed class configuration CRD does not match the controller API")
	}
	served := false
	for _, version := range definition.Spec.Versions {
		if version.Name == configv1alpha1.GroupVersion.Version && version.Served && version.Storage && version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
			served = true
		}
	}
	if !served {
		return fmt.Errorf("the class configuration CRD must serve and store the controller API version with its structural schema")
	}
	for _, condition := range definition.Status.Conditions {
		if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("the class configuration CRD is not established")
}

func int32Pointer(value int32) *int32 { return &value }
