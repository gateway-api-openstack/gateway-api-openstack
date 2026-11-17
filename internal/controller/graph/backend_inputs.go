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

package graph

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

// CollectBackendInputs observes the dependencies of the supplied route inventory.
// The caller establishes inventory completeness and mutation authority separately.
// Reads are shared only within this observation, and a failed read returns no partial input.
func CollectBackendInputs(ctx context.Context, reader client.Reader, gateway *gatewayv1.Gateway, routes []gatewayv1.HTTPRoute) (model.Input, error) {
	if gateway == nil {
		return model.Input{}, fmt.Errorf("gateway is required to collect backend inputs")
	}
	input := model.Input{Gateway: gateway.DeepCopy(), Routes: make([]gatewayv1.HTTPRoute, 0, len(routes))}
	for i := range routes {
		input.Routes = append(input.Routes, *routes[i].DeepCopy())
	}
	slices.SortFunc(input.Routes, func(a, b gatewayv1.HTTPRoute) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name), cmp.Compare(a.UID, b.UID))
	})
	if err := collectRouteNamespaces(ctx, reader, &input); err != nil {
		return model.Input{}, err
	}

	references := backendServiceReferences(input.Routes)
	grantNamespaces := map[string]struct{}{}
	for target, sources := range references {
		for source := range sources {
			if source != target.Namespace {
				grantNamespaces[target.Namespace] = struct{}{}
			}
		}
	}
	for _, namespace := range slices.Sorted(maps.Keys(grantNamespaces)) {
		var grants gatewayv1.ReferenceGrantList
		if err := reader.List(ctx, &grants, client.InNamespace(namespace)); err != nil {
			return model.Input{}, fmt.Errorf("list backend ReferenceGrants in namespace %s: %w", namespace, err)
		}
		input.ReferenceGrants = append(input.ReferenceGrants, grants.DeepCopy().Items...)
	}
	slices.SortFunc(input.ReferenceGrants, func(a, b gatewayv1.ReferenceGrant) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})

	keys := slices.SortedFunc(maps.Keys(references), func(a, b types.NamespacedName) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	for _, key := range keys {
		allowed := false
		for source := range references[key] {
			if model.BackendReferenceAllowed(source, key, input.ReferenceGrants) {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		var service corev1.Service
		if err := reader.Get(ctx, key, &service); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return model.Input{}, fmt.Errorf("get backend Service %s: %w", key, err)
		}
		// Keep deleting Services so callers can distinguish pending deletion from absence.
		input.Services = append(input.Services, *service.DeepCopy())
		if !service.DeletionTimestamp.IsZero() || service.Spec.Type == corev1.ServiceTypeLoadBalancer || service.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		var endpointSlices discoveryv1.EndpointSliceList
		if err := reader.List(ctx, &endpointSlices, client.InNamespace(key.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: key.Name}); err != nil {
			return model.Input{}, fmt.Errorf("list EndpointSlices for Service %s: %w", key, err)
		}
		input.EndpointSlices = append(input.EndpointSlices, endpointSlices.DeepCopy().Items...)
	}
	slices.SortFunc(input.EndpointSlices, func(a, b discoveryv1.EndpointSlice) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return input, nil
}

func backendServiceReferences(routes []gatewayv1.HTTPRoute) map[types.NamespacedName]map[string]struct{} {
	references := map[types.NamespacedName]map[string]struct{}{}
	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			for _, reference := range rule.BackendRefs {
				key, supported := model.BackendServiceReference(route.Namespace, reference.BackendObjectReference)
				if !supported {
					continue
				}
				if references[key] == nil {
					references[key] = map[string]struct{}{}
				}
				references[key][route.Namespace] = struct{}{}
			}
		}
	}
	return references
}

func collectRouteNamespaces(ctx context.Context, reader client.Reader, input *model.Input) error {
	needsLabels := slices.ContainsFunc(input.Gateway.Spec.Listeners, func(listener gatewayv1.Listener) bool {
		if listener.AllowedRoutes == nil || listener.AllowedRoutes.Namespaces == nil {
			return false
		}
		namespaces := listener.AllowedRoutes.Namespaces
		if namespaces.From == nil || *namespaces.From != gatewayv1.NamespacesFromSelector || namespaces.Selector == nil {
			return false
		}
		_, err := metav1.LabelSelectorAsSelector(namespaces.Selector)
		return err == nil
	})
	if !needsLabels {
		return nil
	}
	namespaces := map[string]struct{}{}
	for _, route := range input.Routes {
		namespaces[route.Namespace] = struct{}{}
	}
	for _, name := range slices.Sorted(maps.Keys(namespaces)) {
		var namespace corev1.Namespace
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get HTTPRoute Namespace %s: %w", name, err)
		}
		input.Namespaces = append(input.Namespaces, *namespace.DeepCopy())
	}
	return nil
}
