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

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

func (r *Reconciler) setGatewayFailure(gateway *gatewayv1.Gateway, validationErr error) {
	message := validationErr.Error()
	var detailed *gatewayValidationError
	errors.As(validationErr, &detailed)
	gatewayReason := gatewayv1.GatewayReasonListenersNotValid
	if detailed != nil {
		if detailed.gatewayReason != "" {
			gatewayReason = detailed.gatewayReason
		}
	}
	existingListeners := append([]gatewayv1.ListenerStatus(nil), gateway.Status.Listeners...)
	gateway.Status.Addresses = nil
	controller.SetCondition(&gateway.Status.Conditions, controller.Condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionFalse, string(gatewayReason), message, gateway.Generation))
	controller.SetCondition(&gateway.Status.Conditions, controller.Condition(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse, string(gatewayv1.GatewayReasonInvalid), message, gateway.Generation))
	gateway.Status.Listeners = make([]gatewayv1.ListenerStatus, 0, len(gateway.Spec.Listeners))
	for _, listener := range gateway.Spec.Listeners {
		gateway.Status.Listeners = append(gateway.Status.Listeners, failedListenerStatus(listener, existingListenerConditionsFrom(existingListeners, listener.Name), gateway.Generation, validationErr))
	}
}

func failedListenerStatus(listener gatewayv1.Listener, existing []metav1.Condition, generation int64, validationErr error) gatewayv1.ListenerStatus {
	message := validationErr.Error()
	var detailed *gatewayValidationError
	errors.As(validationErr, &detailed)
	var diagnostics []model.Diagnostic
	listenerSpecific := detailed == nil || detailed.gatewayReason == ""
	if detailed != nil && detailed.listenerDiagnostics != nil {
		diagnostics = detailed.listenerDiagnostics[listener.Name]
		listenerSpecific = len(diagnostics) != 0 || listener.Protocol != gatewayv1.HTTPProtocolType
	}
	invalidRouteKinds := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Reason == string(gatewayv1.ListenerReasonInvalidRouteKinds) {
			invalidRouteKinds = true
			break
		}
	}
	listenerStatus := gatewayv1.ListenerStatus{
		Name:       listener.Name,
		Conditions: existing,
	}
	if listener.Protocol == gatewayv1.HTTPProtocolType && (!invalidRouteKinds || listenerAllowsHTTPRoute(listener)) {
		listenerStatus.SupportedKinds = supportedHTTPRouteKinds()
	}
	acceptedStatus := metav1.ConditionFalse
	acceptedReason := string(gatewayv1.ListenerReasonUnsupportedValue)
	acceptedMessage := message
	programmedReason := string(gatewayv1.ListenerReasonInvalid)
	if !listenerSpecific {
		acceptedStatus = metav1.ConditionTrue
		acceptedReason = string(gatewayv1.ListenerReasonAccepted)
		acceptedMessage = "Listener configuration is accepted"
		programmedReason = string(gatewayv1.ListenerReasonPending)
	} else if listener.Protocol != gatewayv1.HTTPProtocolType {
		acceptedReason = string(gatewayv1.ListenerReasonUnsupportedProtocol)
	}
	conditions := []metav1.Condition{
		controller.Condition(string(gatewayv1.ListenerConditionAccepted), acceptedStatus, acceptedReason, acceptedMessage, generation),
		controller.Condition(string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.ListenerReasonResolvedRefs), "Listener has no unresolved references", generation),
		controller.Condition(string(gatewayv1.ListenerConditionConflicted), metav1.ConditionFalse, string(gatewayv1.ListenerReasonNoConflicts), "Listener has no conflicts", generation),
		controller.Condition(string(gatewayv1.ListenerConditionProgrammed), metav1.ConditionFalse, programmedReason, message, generation),
	}
	for _, diagnostic := range diagnostics {
		status := metav1.ConditionFalse
		if diagnostic.Condition == string(gatewayv1.ListenerConditionConflicted) {
			status = metav1.ConditionTrue
		}
		controller.SetCondition(&conditions, controller.Condition(diagnostic.Condition, status, diagnostic.Reason, diagnostic.Message, generation))
	}
	for _, condition := range conditions {
		controller.SetCondition(&listenerStatus.Conditions, condition)
	}
	return listenerStatus
}

// summarizeRejectedListeners retains valid listener outcomes and adds the
// independently evaluated rejection conditions. Secret payloads never enter
// this request-local status model.
func (r *Reconciler) summarizeRejectedListeners(scope *gatewayScope) {
	evaluation := scope.listeners
	if evaluation.err != nil || len(evaluation.diagnostics) == 0 {
		return
	}
	gateway := scope.gateway
	failure := &gatewayValidationError{message: "Listener configuration cannot be programmed", listenerDiagnostics: evaluation.diagnostics}
	statuses := make([]gatewayv1.ListenerStatus, 0, len(gateway.Spec.Listeners))
	for _, listener := range gateway.Spec.Listeners {
		if len(evaluation.diagnostics[listener.Name]) != 0 {
			existing := existingListenerConditionsFrom(scope.original.Status.Listeners, listener.Name)
			statuses = append(statuses, failedListenerStatus(listener, existing, gateway.Generation, failure))
			continue
		}
		for _, status := range gateway.Status.Listeners {
			if status.Name == listener.Name {
				statuses = append(statuses, status)
				break
			}
		}
	}
	gateway.Status.Listeners = statuses
	controller.SetCondition(&gateway.Status.Conditions, controller.Condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionTrue, string(gatewayv1.GatewayReasonListenersNotValid), "Gateway has valid HTTP listeners and rejected listener configurations", gateway.Generation))
}

func supportedHTTPRouteKinds() []gatewayv1.RouteGroupKind {
	group := gatewayv1.Group(gatewayv1.GroupName)
	return []gatewayv1.RouteGroupKind{{Group: &group, Kind: gatewayv1.Kind("HTTPRoute")}}
}

func existingListenerConditions(gateway *gatewayv1.Gateway, listenerName gatewayv1.SectionName) []metav1.Condition {
	return existingListenerConditionsFrom(gateway.Status.Listeners, listenerName)
}

func existingListenerConditionsFrom(statuses []gatewayv1.ListenerStatus, listenerName gatewayv1.SectionName) []metav1.Condition {
	for _, status := range statuses {
		if status.Name == listenerName {
			return append([]metav1.Condition(nil), status.Conditions...)
		}
	}
	return nil
}

func listenerStatusesWithoutControllerConditions(gateway *gatewayv1.Gateway) []gatewayv1.ListenerStatus {
	statuses := make([]gatewayv1.ListenerStatus, 0, len(gateway.Status.Listeners))
	for _, existing := range gateway.Status.Listeners {
		conditions := make([]metav1.Condition, 0, len(existing.Conditions))
		for _, candidate := range existing.Conditions {
			if !isControllerOwnedListenerCondition(candidate.Type) {
				conditions = append(conditions, candidate)
			}
		}
		if len(conditions) != 0 {
			statuses = append(statuses, gatewayv1.ListenerStatus{Name: existing.Name, Conditions: conditions})
		}
	}
	return statuses
}

func isControllerOwnedListenerCondition(conditionType string) bool {
	switch conditionType {
	case string(gatewayv1.ListenerConditionAccepted),
		string(gatewayv1.ListenerConditionResolvedRefs),
		string(gatewayv1.ListenerConditionConflicted),
		string(gatewayv1.ListenerConditionProgrammed):
		return true
	default:
		return false
	}
}

func (r *Reconciler) setGatewayFailureReason(gateway *gatewayv1.Gateway, reason gatewayv1.GatewayConditionReason, message string) {
	gateway.Status.Addresses = nil
	gateway.Status.Listeners = listenerStatusesWithoutControllerConditions(gateway)
	controller.SetCondition(&gateway.Status.Conditions, controller.Condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionFalse, string(reason), message, gateway.Generation))
	controller.SetCondition(&gateway.Status.Conditions, controller.Condition(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse, string(gatewayv1.GatewayReasonInvalid), message, gateway.Generation))
}

func (r *Reconciler) setGatewayProgressing(ctx context.Context, gateway *gatewayv1.Gateway, listeners []gatewayv1.Listener, message string) error {
	return r.setGatewayProvisioningStatus(ctx, gateway, listeners, metav1.ConditionFalse, gatewayv1.GatewayReasonNoResources, gatewayv1.ListenerReasonPending, message, true)
}

func (r *Reconciler) handleGatewayProviderFailure(
	ctx context.Context,
	scope *gatewayScope,
	listeners []gatewayv1.Listener,
	providerErr error,
) (ctrl.Result, error) {
	policy, ok := controller.ProviderFailurePolicyFor(providerErr)
	if !ok {
		return ctrl.Result{}, controller.SafeProviderReconcileError(controller.ProviderFailurePolicy{Message: "The Gateway graph could not be reconciled"}, providerErr)
	}
	gateway := scope.gateway
	gatewayReason, listenerReason := gatewayProviderFailureReasons(policy.Category)
	programmedGeneration := controller.ConditionObservedGeneration(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		gateway.Generation,
		policy.AdvancesObservedGeneration,
	)
	transitioned := controller.ConditionTransitioned(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		policy.ConditionStatus,
		string(gatewayReason),
		policy.Message,
		programmedGeneration,
	)
	if err := r.setGatewayProvisioningStatus(
		ctx,
		gateway,
		listeners,
		policy.ConditionStatus,
		gatewayReason,
		listenerReason,
		policy.Message,
		policy.AdvancesObservedGeneration,
	); err != nil {
		return ctrl.Result{}, errors.Join(controller.SafeProviderReconcileError(policy, providerErr), err)
	}
	if transitioned {
		scope.queueWarning(policy, "EnsureGateway")
	}
	if !policy.ReturnError {
		scope.patchCause = controller.SafeProviderReconcileError(policy, providerErr)
	}
	return controller.ProviderFailureResult(policy, providerErr, string(gateway.UID))
}

func (r *Reconciler) handleGatewayDeleteFailure(
	scope *gatewayScope,
	providerErr error,
) (ctrl.Result, error) {
	policy, ok := controller.ProviderFailurePolicyFor(providerErr)
	if !ok {
		return ctrl.Result{}, controller.SafeProviderReconcileError(controller.ProviderFailurePolicy{Message: "The Gateway graph could not be removed"}, providerErr)
	}
	gateway := scope.gateway
	policy = controller.ProviderCleanupFailurePolicy(policy, "Gateway")
	gatewayReason, listenerReason := gatewayProviderFailureReasons(policy.Category)
	programmedGeneration := controller.ConditionObservedGeneration(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		gateway.Generation,
		policy.AdvancesObservedGeneration,
	)
	transitioned := controller.ConditionTransitioned(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		policy.ConditionStatus,
		string(gatewayReason),
		policy.Message,
		programmedGeneration,
	)
	r.setGatewayCleanupFailure(gateway, policy, gatewayReason, listenerReason)
	if transitioned {
		scope.queueWarning(policy, "DeleteGateway")
	}
	if !policy.ReturnError {
		scope.patchCause = controller.SafeProviderReconcileError(policy, providerErr)
	}
	return controller.ProviderFinalizationFailureResult(policy, providerErr, string(gateway.UID))
}

func gatewayProviderFailureReasons(category cloud.ErrorCategory) (gatewayv1.GatewayConditionReason, gatewayv1.ListenerConditionReason) {
	switch category {
	case cloud.ErrorCategoryQuota:
		return gatewayv1.GatewayReasonNoResources, gatewayv1.ListenerReasonPending
	case cloud.ErrorCategoryTerminalValidation:
		return gatewayv1.GatewayReasonInvalid, gatewayv1.ListenerReasonInvalid
	case cloud.ErrorCategoryResourceFailure:
		return gatewayv1.GatewayConditionReason("ResourceFailed"), gatewayv1.ListenerConditionReason("ResourceFailed")
	case cloud.ErrorCategoryOwnershipConflict:
		return gatewayv1.GatewayConditionReason("OwnershipConflict"), gatewayv1.ListenerConditionReason("OwnershipConflict")
	default:
		return gatewayv1.GatewayReasonPending, gatewayv1.ListenerReasonPending
	}
}

func (r *Reconciler) setGatewayCleanupFailure(
	gateway *gatewayv1.Gateway,
	policy controller.ProviderFailurePolicy,
	gatewayReason gatewayv1.GatewayConditionReason,
	listenerReason gatewayv1.ListenerConditionReason,
) {
	programmedGeneration := controller.ConditionObservedGeneration(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		gateway.Generation,
		policy.AdvancesObservedGeneration,
	)
	controller.SetCondition(&gateway.Status.Conditions, controller.Condition(
		string(gatewayv1.GatewayConditionProgrammed),
		policy.ConditionStatus,
		string(gatewayReason),
		policy.Message,
		programmedGeneration,
	))
	for index := range gateway.Status.Listeners {
		listenerGeneration := controller.ConditionObservedGeneration(
			gateway.Status.Listeners[index].Conditions,
			string(gatewayv1.ListenerConditionProgrammed),
			gateway.Generation,
			policy.AdvancesObservedGeneration,
		)
		controller.SetCondition(&gateway.Status.Listeners[index].Conditions, controller.Condition(
			string(gatewayv1.ListenerConditionProgrammed),
			policy.ConditionStatus,
			string(listenerReason),
			policy.Message,
			listenerGeneration,
		))
	}
}

func (r *Reconciler) setGatewayProvisioningStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listeners []gatewayv1.Listener,
	programmed metav1.ConditionStatus,
	gatewayReason gatewayv1.GatewayConditionReason,
	listenerReason gatewayv1.ListenerConditionReason,
	message string,
	advanceProgrammedGeneration bool,
) error {
	counts, err := r.attachedRouteCounts(ctx, gateway)
	if err != nil {
		return err
	}
	programmedGeneration := controller.ConditionObservedGeneration(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
		gateway.Generation,
		advanceProgrammedGeneration,
	)
	controller.SetCondition(&gateway.Status.Conditions, controller.Condition(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionTrue, string(gatewayv1.GatewayReasonAccepted), "Gateway configuration is accepted", gateway.Generation))
	controller.SetCondition(&gateway.Status.Conditions, controller.Condition(string(gatewayv1.GatewayConditionProgrammed), programmed, string(gatewayReason), message, programmedGeneration))
	statuses := make([]gatewayv1.ListenerStatus, 0, len(listeners))
	for _, listener := range listeners {
		status := gatewayv1.ListenerStatus{
			Name:           listener.Name,
			SupportedKinds: supportedHTTPRouteKinds(),
			AttachedRoutes: counts[listener.Name],
			Conditions:     existingListenerConditions(gateway, listener.Name),
		}
		setAcceptedListenerConditions(&status.Conditions, gateway.Generation)
		generation := controller.ConditionObservedGeneration(status.Conditions, string(gatewayv1.ListenerConditionProgrammed), gateway.Generation, advanceProgrammedGeneration)
		controller.SetCondition(&status.Conditions, controller.Condition(string(gatewayv1.ListenerConditionProgrammed), programmed, string(listenerReason), message, generation))
		statuses = append(statuses, status)
	}
	gateway.Status.Listeners = statuses
	return nil
}

func (r *Reconciler) setGatewayProgrammed(ctx context.Context, gateway *gatewayv1.Gateway, listeners []gatewayv1.Listener, gatewayState cloud.GatewayState) error {
	if err := r.setGatewayProvisioningStatus(ctx, gateway, listeners, metav1.ConditionTrue, gatewayv1.GatewayReasonProgrammed, gatewayv1.ListenerReasonProgrammed, "Octavia load balancer and listeners are active", true); err != nil {
		return err
	}
	addressType := gatewayv1.IPAddressType
	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Type: &addressType, Value: gatewayState.Address()}}
	return nil
}

func setAcceptedListenerConditions(conditions *[]metav1.Condition, generation int64) {
	controller.SetCondition(conditions, controller.Condition(string(gatewayv1.ListenerConditionAccepted), metav1.ConditionTrue, string(gatewayv1.ListenerReasonAccepted), "Listener is accepted", generation))
	controller.SetCondition(conditions, controller.Condition(string(gatewayv1.ListenerConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.ListenerReasonResolvedRefs), "Listener references are resolved", generation))
	controller.SetCondition(conditions, controller.Condition(string(gatewayv1.ListenerConditionConflicted), metav1.ConditionFalse, string(gatewayv1.ListenerReasonNoConflicts), "Listener has no conflicts", generation))
}

// attachedRouteCounts uses evaluated parent status and current attachment inputs.
// A Route contributes at most once to each logical listener even when several
// ParentRefs select it.
func (r *Reconciler) attachedRouteCounts(ctx context.Context, gateway *gatewayv1.Gateway) (map[gatewayv1.SectionName]int32, error) {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{
		controller.IndexHTTPRouteByStatusGateway: controller.ObjectKeyString(client.ObjectKeyFromObject(gateway)),
	}); err != nil {
		return nil, err
	}
	input := model.Input{Gateway: gateway}
	for i := range routes.Items {
		route := routes.Items[i].DeepCopy()
		route.Spec.ParentRefs = nil
		for _, parent := range routes.Items[i].Spec.ParentRefs {
			for _, status := range route.Status.Parents {
				if status.ControllerName != r.Config.ControllerName || !controller.ParentRefsEqual(parent, route.Namespace, status.ParentRef, route.Namespace) {
					continue
				}
				accepted := meta.FindStatusCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
				if accepted != nil && accepted.Status == metav1.ConditionTrue && accepted.ObservedGeneration == route.Generation {
					route.Spec.ParentRefs = append(route.Spec.ParentRefs, parent)
				}
				break
			}
		}
		if len(route.Spec.ParentRefs) != 0 {
			input.Routes = append(input.Routes, *route)
		}
	}
	if gatewayUsesNamespaceSelector(gateway) {
		seen := make(map[string]bool)
		for _, route := range input.Routes {
			if seen[route.Namespace] {
				continue
			}
			var namespace corev1.Namespace
			if err := r.Get(ctx, client.ObjectKey{Name: route.Namespace}, &namespace); err != nil {
				return nil, err
			}
			input.Namespaces = append(input.Namespaces, namespace)
			seen[route.Namespace] = true
		}
	}
	logical, err := model.Compile(input)
	if err != nil {
		return nil, err
	}
	counts := make(map[gatewayv1.SectionName]int32, len(logical.Listeners))
	for _, listener := range logical.Listeners {
		counts[listener.Name] = listener.AttachedRoutes
	}
	return counts, nil
}

func gatewayUsesNamespaceSelector(gateway *gatewayv1.Gateway) bool {
	for _, listener := range gateway.Spec.Listeners {
		if allowed := listener.AllowedRoutes; allowed != nil && allowed.Namespaces != nil && allowed.Namespaces.From != nil && *allowed.Namespaces.From == gatewayv1.NamespacesFromSelector {
			return true
		}
	}
	return false
}
