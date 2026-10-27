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

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/diagnostics"
)

func readInputs(ctx context.Context, options commandOptions) (diagnostics.Request, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = options.kubeconfig
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: options.kubeContext}).ClientConfig()
	if err != nil {
		return diagnostics.Request{}, err
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return diagnostics.Request{}, err
	}
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		return diagnostics.Request{}, err
	}
	reader, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return diagnostics.Request{}, err
	}
	return collectInputs(ctx, reader, options)
}

func collectInputs(ctx context.Context, reader client.Reader, options commandOptions) (diagnostics.Request, error) {
	parameters := &configv1alpha1.OpenStackGatewayClassConfig{}
	if err := reader.Get(ctx, types.NamespacedName{Name: options.classConfig}, parameters); err != nil {
		return diagnostics.Request{}, fmt.Errorf("read class configuration: %w", err)
	}
	if !parameters.DeletionTimestamp.IsZero() {
		return diagnostics.Request{}, fmt.Errorf("class configuration is being deleted")
	}
	topologyRequest, addressType, _, err := classconfig.Validate(parameters.Spec)
	if err != nil {
		return diagnostics.Request{}, err
	}
	nodes := &corev1.NodeList{}
	if err := reader.List(ctx, nodes, client.MatchingLabelsSelector{Selector: options.nodeSelector}); err != nil {
		return diagnostics.Request{}, fmt.Errorf("list worker Nodes: %w", err)
	}
	slices.SortFunc(nodes.Items, func(left, right corev1.Node) int { return strings.Compare(left.Name, right.Name) })
	request := diagnostics.Request{Topology: topologyRequest, CheckBarbican: options.barbican}
	for index := range nodes.Items {
		node := &nodes.Items[index]
		if controller.NodePortNodeExcluded(node) {
			continue
		}
		// Structural diagnostics include unready workers. They do not claim that
		// a worker is an eligible member of a particular Service or route.
		request.Nodes = append(request.Nodes, diagnostics.Node{ProviderID: node.Spec.ProviderID, Address: selectedAddress(node, addressType)})
	}
	return request, nil
}

func selectedAddress(node *corev1.Node, addressType corev1.NodeAddressType) string {
	var selected string
	for _, address := range node.Status.Addresses {
		if address.Type != addressType {
			continue
		}
		if selected != "" && selected != address.Address {
			return ""
		}
		selected = address.Address
	}
	return selected
}
