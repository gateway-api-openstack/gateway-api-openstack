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
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func kubernetesConfig(config Config) (*rest.Config, error) {
	raw, err := clientcmd.LoadFromFile(config.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("read explicit kubeconfig: %w", err)
	}
	selected := raw.Contexts[config.Context]
	if selected == nil || raw.Clusters[selected.Cluster] == nil {
		return nil, fmt.Errorf("explicit context or its cluster is missing")
	}
	if strings.TrimRight(raw.Clusters[selected.Cluster].Server, "/") != strings.TrimRight(config.ExpectedAPIServer, "/") {
		return nil, fmt.Errorf("context does not select the expected API server")
	}
	return clientcmd.NewNonInteractiveClientConfig(*raw, config.Context, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
}

// preflight is read-only. Namespace absence is checked before the upstream
// applier, which otherwise updates and later deletes pre-existing fixtures.
func preflight(ctx context.Context, reader client.Reader, config Config, fixtures []client.Object) (*gatewayv1.GatewayClass, error) {
	var system corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: "kube-system"}, &system); err != nil {
		return nil, fmt.Errorf("read cluster identity: %w", err)
	}
	if string(system.UID) != config.ExpectedClusterUID || !system.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("kube-system UID does not match the expected cluster")
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := reader.Get(ctx, client.ObjectKey{Name: config.GatewayClassName}, &gatewayClass); err != nil {
		return nil, fmt.Errorf("read GatewayClass: %w", err)
	}
	if string(gatewayClass.Spec.ControllerName) != config.ControllerName || !gatewayClass.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("GatewayClass is deleting or selects a different controller")
	}
	accepted := meta.FindStatusCondition(gatewayClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.ObservedGeneration != gatewayClass.Generation {
		return nil, fmt.Errorf("GatewayClass has not accepted its current generation")
	}
	if err := validateDeclaredFeatures(config, &gatewayClass); err != nil {
		return nil, err
	}
	parameters := gatewayClass.Spec.ParametersRef
	if parameters == nil || parameters.Group != "gateway-api-openstack.github.io" || parameters.Kind != "OpenStackGatewayClassConfig" || parameters.Namespace != nil {
		return nil, fmt.Errorf("GatewayClass must reference a cluster-scoped OpenStackGatewayClassConfig")
	}
	classConfig := &unstructured.Unstructured{}
	classConfig.SetGroupVersionKind(schema.GroupVersionKind{Group: string(parameters.Group), Version: "v1alpha1", Kind: string(parameters.Kind)})
	if err := reader.Get(ctx, client.ObjectKey{Name: parameters.Name}, classConfig); err != nil {
		return nil, fmt.Errorf("read class configuration: %w", err)
	}
	mode, _, err := unstructured.NestedString(classConfig.Object, "spec", "memberMode")
	if err != nil || mode != "Endpoint" || !classConfig.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("unmodified upstream Service fixtures require an active Endpoint class configuration")
	}
	var crds apiextensionsv1.CustomResourceDefinitionList
	if err := reader.List(ctx, &crds); err != nil {
		return nil, fmt.Errorf("read Gateway API bundle: %w", err)
	}
	required := sets.New("gateways.gateway.networking.k8s.io", "gatewayclasses.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io", "referencegrants.gateway.networking.k8s.io")
	for _, crd := range crds.Items {
		if crd.Spec.Group != gatewayv1.GroupName {
			continue
		}
		if crd.Annotations["gateway.networking.k8s.io/bundle-version"] != bundleVersion || crd.Annotations["gateway.networking.k8s.io/channel"] != "standard" {
			return nil, fmt.Errorf("installed Gateway API CRDs must be the pinned %s Standard Channel bundle", bundleVersion)
		}
		required.Delete(crd.Name)
	}
	if len(required) != 0 {
		return nil, fmt.Errorf("required Gateway API CRDs are missing")
	}
	for _, fixture := range fixtures {
		if err := reader.Get(ctx, client.ObjectKeyFromObject(fixture), fixture.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
			if err != nil {
				return nil, fmt.Errorf("check fixture isolation: %w", err)
			}
			return nil, fmt.Errorf("upstream fixture %s already exists; inspect and clean the prior run before proceeding", client.ObjectKeyFromObject(fixture))
		}
	}
	return &gatewayClass, nil
}

func validateDeclaredFeatures(config Config, gatewayClass *gatewayv1.GatewayClass) error {
	if config.Mode == diagnosticMode {
		return nil
	}
	declared := sets.New[features.FeatureName]()
	for _, feature := range gatewayClass.Status.SupportedFeatures {
		declared.Insert(features.FeatureName(feature.Name))
	}
	profile := suite.GatewayHTTPConformanceProfile
	if !declared.IsSuperset(profile.CoreFeatures) {
		return fmt.Errorf("profile mode requires the complete Core feature declaration in GatewayClass status; use diagnostic mode for implementation gaps")
	}
	if !profile.CoreFeatures.Union(profile.ExtendedFeatures).IsSuperset(declared) {
		return fmt.Errorf("this runner supports only GATEWAY-HTTP features")
	}
	return nil
}
