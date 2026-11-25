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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

func (r *Reconciler) observeParentAttachments(ctx context.Context, scope *httpRouteScope) error {
	attachments, err := routeParentAttachments(ctx, r.Client, scope.route, scope.parents[0].gateway)
	if err != nil {
		return err
	}
	scope.attachments = attachments
	return nil
}

// routeParentAttachments evaluates attachment without interpreting unresolved
// backend diagnostics. Live graph collection later seals the complete inputs.
func routeParentAttachments(ctx context.Context, reader client.Reader, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) ([]model.ParentAttachment, error) {
	input := model.Input{Gateway: gateway, Routes: []gatewayv1.HTTPRoute{*route}}
	for _, listener := range gateway.Spec.Listeners {
		if listener.AllowedRoutes == nil || listener.AllowedRoutes.Namespaces == nil || listener.AllowedRoutes.Namespaces.From == nil || *listener.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSelector {
			continue
		}
		var namespace corev1.Namespace
		if err := reader.Get(ctx, types.NamespacedName{Name: route.Namespace}, &namespace); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("get Route namespace for attachment: %w", err)
			}
		} else {
			input.Namespaces = []corev1.Namespace{namespace}
		}
		break
	}
	logical, err := model.Compile(input)
	if err != nil {
		return nil, err
	}
	if len(logical.Routes) == 0 {
		return nil, nil
	}
	return logical.Routes[0].Parents, nil
}

func hasAcceptedAttachment(attachments []model.ParentAttachment) bool {
	for _, parent := range attachments {
		if parent.State == model.AttachmentAccepted {
			return true
		}
	}
	return false
}
