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

package gateway

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

// collectListenerInputs observes only authorized certificate references for this
// request. Secret payloads bypass the shared cache and never enter bindings.
// This read path does not authorize certificate publication or HTTPS forwarding.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=list;watch
func (r *Reconciler) collectListenerInputs(ctx context.Context, gateway *gatewayv1.Gateway) (model.Input, error) {
	input := model.Input{Gateway: gateway}
	var references []gatewayv1.SecretObjectReference
	for _, listener := range gateway.Spec.Listeners {
		configuration := listener.TLS
		if listener.Protocol != gatewayv1.HTTPSProtocolType || configuration == nil ||
			ptr.Deref(configuration.Mode, gatewayv1.TLSModeTerminate) != gatewayv1.TLSModeTerminate ||
			len(configuration.Options) != 0 || len(configuration.CertificateRefs) != 1 {
			continue
		}
		references = append(references, configuration.CertificateRefs[0])
	}
	if len(references) == 0 {
		return input, nil
	}
	if r.APIReader == nil {
		return model.Input{}, controller.ErrAPIReaderRequired
	}
	if err := r.validateListenerInputOwnership(ctx, gateway); err != nil {
		return model.Input{}, err
	}

	grants := make(map[string][]gatewayv1.ReferenceGrant)
	observed := make(map[types.NamespacedName]struct{})
	for _, reference := range references {
		key, supported := model.CertificateSecretReference(gateway.Namespace, reference)
		if !supported || key.Namespace == "" || key.Name == "" {
			continue
		}
		if key.Namespace != gateway.Namespace {
			if _, found := grants[key.Namespace]; !found {
				var list gatewayv1.ReferenceGrantList
				if err := r.APIReader.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
					return model.Input{}, fmt.Errorf("list certificate ReferenceGrants: %w", err)
				}
				grants[key.Namespace] = list.Items
				input.ReferenceGrants = append(input.ReferenceGrants, list.Items...)
			}
			if !model.CertificateReferenceAllowed(gateway.Namespace, reference, grants[key.Namespace]) {
				continue
			}
		}
		if _, found := observed[key]; found {
			continue
		}
		observed[key] = struct{}{}
		var secret corev1.Secret
		if err := r.APIReader.Get(ctx, key, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return model.Input{}, fmt.Errorf("get certificate Secret: %w", err)
		}
		input.Secrets = append(input.Secrets, secret)
	}
	return input, nil
}

func (r *Reconciler) validateListenerInputOwnership(ctx context.Context, expected *gatewayv1.Gateway) error {
	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &gateway); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expected.UID || gateway.Generation != expected.Generation || !gateway.DeletionTimestamp.IsZero() {
		return errGatewayChanged
	}
	var class gatewayv1.GatewayClass
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: string(gateway.Spec.GatewayClassName)}, &class); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if class.Spec.ControllerName != r.Config.ControllerName {
		return errGatewayChanged
	}
	return nil
}
