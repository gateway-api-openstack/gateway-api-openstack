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

package diagnostic

import (
	"context"
	"fmt"
	"strings"
	"time"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

func restConfig(cfg Config) (*rest.Config, error) {
	raw, err := clientcmd.LoadFromFile(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load explicit kubeconfig: %w", err)
	}
	selected, ok := raw.Contexts[cfg.Context]
	if !ok || selected == nil {
		return nil, fmt.Errorf("explicit kubeconfig context does not exist")
	}
	cluster, ok := raw.Clusters[selected.Cluster]
	if !ok || cluster == nil || strings.TrimRight(cluster.Server, "/") != strings.TrimRight(cfg.APIServer, "/") {
		return nil, fmt.Errorf("selected context does not match the expected API server")
	}
	result, err := clientcmd.NewNonInteractiveClientConfig(*raw, cfg.Context, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("resolve explicit kubeconfig context: %w", err)
	}
	result.Timeout = 30 * time.Second
	result.QPS, result.Burst = 2, 4
	result.UserAgent = "gateway-api-openstack-conformance-diagnostic/" + upstreamVersion
	return result, nil
}

func preflight(ctx context.Context, reader client.Reader, cfg Config) error {
	var system corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: "kube-system"}, &system); err != nil {
		return fmt.Errorf("read cluster identity: %w", err)
	}
	if string(system.UID) != cfg.ClusterUID || !system.DeletionTimestamp.IsZero() {
		return fmt.Errorf("cluster identity does not match the approved cluster")
	}
	var namespace corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: cfg.Namespace}, &namespace); !apierrors.IsNotFound(err) {
		if err != nil {
			return fmt.Errorf("check diagnostic namespace absence: %w", err)
		}
		return fmt.Errorf("diagnostic namespace already exists; it will not be adopted")
	}
	if err := requireCRDs(ctx, reader); err != nil {
		return err
	}
	if _, err := requireClass(ctx, reader, cfg); err != nil {
		return err
	}
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes, client.MatchingLabels(cfg.BackendNodeSelector)); err != nil {
		return fmt.Errorf("read selected backend Nodes: %w", err)
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if controller.NodePortNodeExcluded(node) || node.Spec.Unschedulable || !node.DeletionTimestamp.IsZero() {
			continue
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				return nil
			}
		}
	}
	return fmt.Errorf("backend selector has no eligible ready worker")
}

func requireClass(ctx context.Context, reader client.Reader, cfg Config) (corev1.NodeAddressType, error) {
	var class gatewayv1.GatewayClass
	if err := reader.Get(ctx, client.ObjectKey{Name: cfg.GatewayClass}, &class); err != nil {
		return "", fmt.Errorf("read approved GatewayClass: %w", err)
	}
	if string(class.UID) != cfg.GatewayClassUID || string(class.Spec.ControllerName) != cfg.ControllerName || !class.DeletionTimestamp.IsZero() {
		return "", fmt.Errorf("GatewayClass identity or controller does not match")
	}
	for _, kind := range []string{string(gatewayv1.GatewayClassConditionStatusAccepted), string(gatewayv1.GatewayClassConditionStatusSupportedVersion)} {
		condition := meta.FindStatusCondition(class.Status.Conditions, kind)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != class.Generation {
			return "", fmt.Errorf("GatewayClass is not currently accepted with a supported bundle")
		}
	}
	ref := class.Spec.ParametersRef
	if ref == nil || ref.Group != gatewayv1.Group(configv1alpha1.GroupName) || ref.Kind != gatewayv1.Kind(configv1alpha1.GatewayClassConfigKind) || ref.Name != cfg.ClassConfig || ref.Namespace != nil {
		return "", fmt.Errorf("GatewayClass does not reference the approved cluster-scoped class configuration")
	}
	var parameters configv1alpha1.OpenStackGatewayClassConfig
	if err := reader.Get(ctx, client.ObjectKey{Name: cfg.ClassConfig}, &parameters); err != nil {
		return "", fmt.Errorf("read approved class configuration: %w", err)
	}
	if string(parameters.UID) != cfg.ClassConfigUID || !parameters.DeletionTimestamp.IsZero() {
		return "", fmt.Errorf("class configuration identity does not match")
	}
	topology, addressType, _, err := classconfig.Validate(parameters.Spec)
	if err != nil {
		return "", fmt.Errorf("class configuration is invalid")
	}
	if parameters.Spec.FloatingIP.Mode != configv1alpha1.FloatingIPNone || topology.ExternalNetwork != nil {
		return "", fmt.Errorf("this diagnostic requires explicit floatingIP.mode None")
	}
	return addressType, nil
}

func requireCRDs(ctx context.Context, reader client.Reader) error {
	observed, err := controller.ObserveGatewayAPIVersion(ctx, reader)
	if err != nil {
		return err
	}
	if !observed.Supported {
		return fmt.Errorf("installed Gateway API CRDs must use the pinned bundle")
	}
	var metadata metav1.PartialObjectMetadataList
	metadata.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinitionList"))
	if err := reader.List(ctx, &metadata); err != nil {
		return fmt.Errorf("read Gateway API channel metadata: %w", err)
	}
	for _, definition := range metadata.Items {
		if controller.IsGatewayAPICRDName(definition.Name) && definition.Annotations[gatewayconsts.ChannelAnnotation] != "standard" {
			return fmt.Errorf("installed Gateway API CRDs must use the Standard Channel")
		}
	}
	var definition apiextensionsv1.CustomResourceDefinition
	if err := reader.Get(ctx, client.ObjectKey{Name: "openstackgatewayclassconfigs." + configv1alpha1.GroupName}, &definition); err != nil {
		return fmt.Errorf("read operator-installed class configuration CRD: %w", err)
	}
	if definition.Spec.Group != configv1alpha1.GroupName || definition.Spec.Names.Kind != configv1alpha1.GatewayClassConfigKind || definition.Spec.Scope != apiextensionsv1.ClusterScoped || !definition.DeletionTimestamp.IsZero() {
		return fmt.Errorf("class configuration CRD identity does not match")
	}
	established := false
	for _, condition := range definition.Status.Conditions {
		if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
			established = true
		}
	}
	for _, version := range definition.Spec.Versions {
		if established && version.Name == configv1alpha1.GroupVersion.Version && version.Served && version.Storage && version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
			return nil
		}
	}
	return fmt.Errorf("the operator-installed class configuration CRD is not ready")
}
