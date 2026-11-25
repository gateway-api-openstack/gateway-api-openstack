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
	"fmt"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type gatewayValidationError struct {
	message             string
	gatewayReason       gatewayv1.GatewayConditionReason
	listenerDiagnostics map[gatewayv1.SectionName][]model.Diagnostic
}

func (e *gatewayValidationError) Error() string { return e.message }

func (r *Reconciler) getGatewayClass(ctx context.Context, gateway *gatewayv1.Gateway) (*gatewayv1.GatewayClass, bool, error) {
	var gatewayClass gatewayv1.GatewayClass
	key := types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}
	if err := r.Get(ctx, key, &gatewayClass); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get GatewayClass %q: %w", key.Name, err)
	}
	return &gatewayClass, gatewayClass.Spec.ControllerName == r.Config.ControllerName, nil
}

// Validate returns the listeners that can contribute to the HTTP graph.
// A rejected sibling cannot authorize deleting a valid listener's resources.
func Validate(gateway *gatewayv1.Gateway) ([]gatewayv1.Listener, error) {
	evaluation := evaluateGatewayListeners(model.Input{Gateway: gateway})
	return evaluation.listeners, evaluation.err
}

type gatewayListenerEvaluation struct {
	listeners   []gatewayv1.Listener
	diagnostics map[gatewayv1.SectionName][]model.Diagnostic
	err         error
}

func evaluateGatewayListeners(input model.Input) gatewayListenerEvaluation {
	gateway := input.Gateway
	evaluation := gatewayListenerEvaluation{}
	if err := validateGatewayFields(gateway); err != nil {
		evaluation.err = err
		return evaluation
	}
	logical, err := model.Compile(input)
	if err != nil {
		evaluation.err = err
		return evaluation
	}
	evaluation.diagnostics = make(map[gatewayv1.SectionName][]model.Diagnostic)
	accepted := make(map[gatewayv1.SectionName]bool)
	for _, listener := range logical.Listeners {
		diagnostics := listener.Diagnostics
		if listener.Protocol != gatewayv1.HTTPProtocolType {
			diagnostics = append(diagnostics, model.Diagnostic{
				Condition: string(gatewayv1.ListenerConditionAccepted), Reason: string(gatewayv1.ListenerReasonUnsupportedProtocol),
				Message: "only plain HTTP listeners can be programmed",
			})
		}
		if len(diagnostics) != 0 {
			evaluation.diagnostics[listener.Name] = diagnostics
			continue
		}
		accepted[listener.Name] = true
	}
	for _, listener := range gateway.Spec.Listeners {
		if accepted[listener.Name] {
			evaluation.listeners = append(evaluation.listeners, listener)
		}
	}
	if len(evaluation.listeners) == 0 {
		evaluation.err = &gatewayValidationError{
			message:             "Gateway has no valid HTTP listeners",
			listenerDiagnostics: evaluation.diagnostics,
		}
	}
	return evaluation
}

func validateGatewayFields(gateway *gatewayv1.Gateway) error {
	if len(gateway.Spec.Addresses) != 0 {
		return &gatewayValidationError{message: "spec.addresses is not supported", gatewayReason: gatewayv1.GatewayReasonUnsupportedAddress}
	}
	if gateway.Spec.Infrastructure != nil {
		return &gatewayValidationError{message: "spec.infrastructure is not supported", gatewayReason: gatewayv1.GatewayReasonInvalidParameters}
	}
	if gateway.Spec.AllowedListeners != nil {
		return &gatewayValidationError{message: "spec.allowedListeners and ListenerSet attachment are not supported", gatewayReason: gatewayv1.GatewayReasonInvalidParameters}
	}
	if gateway.Spec.TLS != nil {
		return &gatewayValidationError{message: "gateway-wide spec.tls is not supported", gatewayReason: gatewayv1.GatewayReasonInvalidParameters}
	}
	if len(gateway.Spec.Listeners) == 0 {
		return fmt.Errorf("at least one HTTP listener is required")
	}
	return nil
}

func listenerAllowsHTTPRoute(listener gatewayv1.Listener) bool {
	if listener.AllowedRoutes == nil || len(listener.AllowedRoutes.Kinds) == 0 {
		return true
	}
	for _, kind := range listener.AllowedRoutes.Kinds {
		if isHTTPRouteGroupKind(kind) {
			return true
		}
	}
	return false
}

func isHTTPRouteGroupKind(routeKind gatewayv1.RouteGroupKind) bool {
	group := gatewayv1.Group(gatewayv1.GroupName)
	if routeKind.Group != nil {
		group = *routeKind.Group
	}
	return group == gatewayv1.Group(gatewayv1.GroupName) && routeKind.Kind == gatewayv1.Kind("HTTPRoute")
}
