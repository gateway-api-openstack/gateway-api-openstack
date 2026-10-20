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
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// RequireGatewayAPICRDs verifies the pinned Standard bundle without changing it.
func RequireGatewayAPICRDs(ctx context.Context, reader client.Reader) error {
	var definitions metav1.PartialObjectMetadataList
	definitions.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinitionList"))
	if err := reader.List(ctx, &definitions); err != nil {
		return fmt.Errorf("list installed Gateway API CRDs: %w", err)
	}
	present := make(map[string]struct{})
	for _, definition := range definitions.Items {
		if !controller.IsGatewayAPICRDName(definition.Name) {
			continue
		}
		present[definition.Name] = struct{}{}
		if !definition.DeletionTimestamp.IsZero() ||
			definition.Annotations[gatewayconsts.BundleVersionAnnotation] != gatewayconsts.BundleVersion ||
			definition.Annotations[gatewayconsts.ChannelAnnotation] != "standard" {
			return fmt.Errorf("an installed Gateway API CRD is not from the pinned Standard bundle or is being deleted")
		}
	}
	for _, name := range []string{
		"gatewayclasses.gateway.networking.k8s.io",
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
	} {
		if _, found := present[name]; !found {
			return fmt.Errorf("a required Gateway API CRD is absent")
		}
	}
	return nil
}
