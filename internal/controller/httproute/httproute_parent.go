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

package httproute

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

type managedRouteParent struct {
	ref          gatewayv1.ParentReference
	gateway      *gatewayv1.Gateway
	gatewayClass *gatewayv1.GatewayClass
}

func (r *Reconciler) managedParents(ctx context.Context, route *gatewayv1.HTTPRoute) ([]managedRouteParent, error) {
	return r.managedParentsWithReader(ctx, r.Client, route)
}

func (r *Reconciler) managedParentsWithReader(
	ctx context.Context,
	reader client.Reader,
	route *gatewayv1.HTTPRoute,
) ([]managedRouteParent, error) {
	parents := make([]managedRouteParent, 0, len(route.Spec.ParentRefs))
	for _, parentRef := range route.Spec.ParentRefs {
		if !controller.IsGatewayParentRef(parentRef) {
			continue
		}
		namespace := route.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}
		var gateway gatewayv1.Gateway
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: string(parentRef.Name)}, &gateway); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get parent Gateway: %w", err)
		}
		var class gatewayv1.GatewayClass
		if err := reader.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &class); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get parent GatewayClass: %w", err)
		}
		if class.Spec.ControllerName != r.Config.ControllerName {
			continue
		}
		parents = append(parents, managedRouteParent{ref: parentRef, gateway: &gateway, gatewayClass: &class})
	}
	return parents, nil
}

// validateRouteParent checks whether the requested listener exists and has an
// implemented protocol. Attachment authorization, including namespace selectors
// and hostname intersection, uses the complete logical input before binding.
func validateRouteParent(_ *gatewayv1.HTTPRoute, parent managedRouteParent) error {
	if !parent.gateway.DeletionTimestamp.IsZero() {
		return newRouteBuildError(routeErrorPending, "parent Gateway is being deleted")
	}
	selected := false
	for i := range parent.gateway.Spec.Listeners {
		listener := &parent.gateway.Spec.Listeners[i]
		if !selectsListener(parent.ref, listener) {
			continue
		}
		selected = true
		if listener.Protocol == gatewayv1.HTTPProtocolType && listener.TLS == nil {
			return nil
		}
	}
	if !selected {
		return newRouteBuildError(routeErrorNoMatchingParent, "parentRef does not select a listener")
	}
	return newRouteBuildError(routeErrorUnsupported, "selected listeners do not support plain HTTP forwarding")
}

func selectsListener(parent gatewayv1.ParentReference, listener *gatewayv1.Listener) bool {
	return (parent.SectionName == nil || *parent.SectionName == listener.Name) && (parent.Port == nil || *parent.Port == listener.Port)
}
