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
	"errors"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

type routeStatusPlanKind int

const (
	routeStatusPlanReplace routeStatusPlanKind = iota
	routeStatusPlanProviderFailure
	routeStatusPlanDeletingFailure
)

type routeStatusPlan struct {
	kind                routeStatusPlanKind
	updates             []parentStatusUpdate
	parent              gatewayv1.ParentReference
	policy              controller.ProviderFailurePolicy
	referencesEvaluated bool
	validateOwnership   bool
	managedParents      []gatewayv1.ParentReference
	cleanupIdentity     cloud.Identity
}

type routeStatusPatchResult struct {
	applied      bool
	changed      bool
	transitioned bool
}

type routeProviderWarning struct {
	policy                 controller.ProviderFailurePolicy
	action                 string
	cleanupCheckpointSaved bool
	recordOnStatusChange   bool
}

type httpRouteScope struct {
	identity    cloud.Identity
	parents     []managedRouteParent
	attachments []model.ParentAttachment
	children    []*httpRouteScope
	route       *gatewayv1.HTTPRoute
	statusPlan  *routeStatusPlan
	warning     *routeProviderWarning
	patchCause  error
	skipPatch   bool
}

func (r *Reconciler) newHTTPRouteScope(
	ctx context.Context,
	req ctrl.Request,
) (*httpRouteScope, bool, error) {
	route := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	scope := &httpRouteScope{route: route}
	if route.DeletionTimestamp.IsZero() {
		return scope, true, nil
	}
	return scope, controller.RouteHasControllerBinding(r.Config, route), nil
}

func (s *httpRouteScope) setStatuses(updates []parentStatusUpdate) {
	s.statusPlan = &routeStatusPlan{
		kind:              routeStatusPlanReplace,
		updates:           s.expandParentStatuses(updates),
		validateOwnership: true,
	}
}

func (s *httpRouteScope) setProviderFailureStatuses(
	updates []parentStatusUpdate,
	policy controller.ProviderFailurePolicy,
	referencesEvaluated bool,
) {
	s.statusPlan = &routeStatusPlan{
		kind:                routeStatusPlanProviderFailure,
		updates:             s.expandParentStatuses(updates),
		policy:              policy,
		referencesEvaluated: referencesEvaluated,
		validateOwnership:   true,
	}
}

func (s *httpRouteScope) setDeletingFailureStatus(
	parent gatewayv1.ParentReference,
	identity cloud.Identity,
	policy controller.ProviderFailurePolicy,
) {
	s.statusPlan = &routeStatusPlan{
		kind:            routeStatusPlanDeletingFailure,
		parent:          parent,
		cleanupIdentity: identity,
		policy:          policy,
	}
}

func (s *httpRouteScope) queueWarning(warning routeProviderWarning) {
	s.warning = &warning
}

func (r *Reconciler) patchHTTPRoute(ctx context.Context, scope *httpRouteScope) error {
	var childErr error
	for _, child := range scope.children {
		if err := r.patchHTTPRoute(ctx, child); err != nil {
			childErr = errors.Join(childErr, err, child.patchCause)
		}
	}
	if scope.skipPatch {
		return childErr
	}
	patch := routeStatusPatchResult{applied: true}
	if scope.statusPlan != nil {
		var err error
		patch, err = r.patchHTTPRouteStatus(ctx, scope.route, scope.statusPlan)
		if err != nil {
			return errors.Join(childErr, fmt.Errorf("patch HTTPRoute status: %w", err))
		}
	}
	if patch.applied && scope.warning != nil &&
		(scope.warning.cleanupCheckpointSaved || patch.transitioned || scope.warning.recordOnStatusChange && patch.changed) {
		controller.RecordProviderWarning(r.Recorder, scope.route, scope.warning.policy, scope.warning.action)
	}
	return childErr
}

func (r *Reconciler) patchHTTPRouteStatus(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	plan *routeStatusPlan,
) (result routeStatusPatchResult, err error) {
	key := client.ObjectKeyFromObject(route)
	uid, generation := route.UID, route.Generation
	isDeleting := !route.DeletionTimestamp.IsZero()
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result = routeStatusPatchResult{}
		current := &gatewayv1.HTTPRoute{}
		if err := r.routePatchReader().Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !sameHTTPRouteRevision(current, uid, generation, isDeleting) {
			return errHTTPRouteChanged
		}
		if plan.validateOwnership {
			if err := r.validateRouteStatusOwnership(ctx, current, plan); err != nil {
				return err
			}
		}
		if plan.kind == routeStatusPlanDeletingFailure {
			identity := plan.cleanupIdentity
			var gateway gatewayv1.Gateway
			err := r.routePatchReader().Get(ctx, client.ObjectKey{Namespace: identity.GatewayNamespace, Name: identity.GatewayName}, &gateway)
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("read Gateway before HTTPRoute cleanup status: %w", err)
			}
			if err == nil && string(gateway.UID) != identity.GatewayUID {
				// ParentStatus names a Gateway, not its UID. Keep the old graph's
				// failure in its binding without changing its replacement's status.
				*route = *current
				result.applied = true
				return nil
			}
		}

		base := current.DeepCopy()
		result.transitioned = plan.apply(r.Config, current)
		result.applied = true
		if reflect.DeepEqual(base.Status, current.Status) {
			*route = *current
			return nil
		}
		if err := r.Status().Patch(ctx, current, controller.OptimisticMergeFrom(base)); err != nil {
			return err
		}
		result.changed = true
		*route = *current
		log.FromContext(ctx).V(1).Info("Updated HTTPRoute status")
		return nil
	})
	return result, err
}

func (r *Reconciler) routeStatusCheckpointNeeded(
	ctx context.Context,
	scope *httpRouteScope,
) (bool, error) {
	if scope.statusPlan == nil {
		return false, nil
	}
	current := &gatewayv1.HTTPRoute{}
	if err := r.routePatchReader().Get(ctx, client.ObjectKeyFromObject(scope.route), current); err != nil {
		return false, fmt.Errorf("read HTTPRoute status checkpoint: %w", err)
	}
	if !sameHTTPRouteRevision(
		current,
		scope.route.UID,
		scope.route.Generation,
		!scope.route.DeletionTimestamp.IsZero(),
	) {
		return false, errHTTPRouteChanged
	}
	if scope.statusPlan.validateOwnership {
		if err := r.validateRouteStatusOwnership(ctx, current, scope.statusPlan); err != nil {
			return false, err
		}
	}
	desired := current.DeepCopy()
	scope.statusPlan.apply(r.Config, desired)
	*scope.route = *current
	return !reflect.DeepEqual(current.Status, desired.Status), nil
}

func (r *Reconciler) routeProviderFailureCheckpointExists(
	ctx context.Context,
	scope *httpRouteScope,
	updates []parentStatusUpdate,
) (bool, error) {
	current := &gatewayv1.HTTPRoute{}
	if err := r.routePatchReader().Get(ctx, client.ObjectKeyFromObject(scope.route), current); err != nil {
		return false, fmt.Errorf("read HTTPRoute provider failure checkpoint: %w", err)
	}
	if !sameHTTPRouteRevision(
		current,
		scope.route.UID,
		scope.route.Generation,
		!scope.route.DeletionTimestamp.IsZero(),
	) {
		return false, errHTTPRouteChanged
	}
	*scope.route = *current
	return routeHasProviderFailureStatus(
		current,
		updates,
		r.Config.ControllerName,
		r.Config.Domain()+"/Programmed",
	), nil
}

func (r *Reconciler) routeCleanupStatusCheckpointNeeded(
	ctx context.Context,
	scope *httpRouteScope,
	updates []parentStatusUpdate,
) (bool, error) {
	if routeHasProviderFailureStatus(
		scope.route,
		updates,
		r.Config.ControllerName,
		r.Config.Domain()+"/Programmed",
	) {
		durable, err := r.routeProviderFailureCheckpointExists(ctx, scope, updates)
		if err != nil {
			return false, err
		}
		if durable {
			return false, nil
		}
	}
	scope.setStatuses(updates)
	return r.routeStatusCheckpointNeeded(ctx, scope)
}

func (r *Reconciler) validateRouteStatusOwnership(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	plan *routeStatusPlan,
) error {
	parents, err := r.managedParentsWithReader(ctx, r.routePatchReader(), route)
	if err != nil {
		return err
	}
	plan.managedParents = nil
	for _, parent := range parents {
		plan.managedParents = append(plan.managedParents, parent.ref)
	}
	for _, update := range plan.updates {
		owned := false
		for _, parent := range parents {
			if controller.ParentRefsEqual(parent.ref, route.Namespace, update.parent, route.Namespace) {
				owned = true
				break
			}
		}
		if !owned {
			return errHTTPRouteChanged
		}
	}
	return nil
}

func (r *Reconciler) routePatchReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (p *routeStatusPlan) apply(config controller.Config, route *gatewayv1.HTTPRoute) bool {
	if p.validateOwnership {
		parents := route.Status.Parents[:0]
		for _, status := range route.Status.Parents {
			keep := status.ControllerName != config.ControllerName
			for _, ref := range p.managedParents {
				if controller.ParentRefsEqual(status.ParentRef, route.Namespace, ref, route.Namespace) {
					keep = true
					break
				}
			}
			if keep {
				parents = append(parents, status)
			}
		}
		route.Status.Parents = parents
	}
	switch p.kind {
	case routeStatusPlanReplace:
		applyRouteParentStatuses(config, route, p.updates)
		return false
	case routeStatusPlanProviderFailure:
		return applyRouteProviderFailureStatuses(
			config,
			route,
			p.updates,
			p.policy,
			p.referencesEvaluated,
		)
	case routeStatusPlanDeletingFailure:
		return applyDeletingRouteProviderFailure(config, route, p.parent, p.policy)
	default:
		return false
	}
}

func applyRouteParentStatuses(config controller.Config, route *gatewayv1.HTTPRoute, updates []parentStatusUpdate) {
	mergeRouteParentStatuses(config, route, updates, func(parent *gatewayv1.RouteParentStatus, status routeReconcileStatus) bool {
		controller.SetCondition(&parent.Conditions, controller.Condition(string(gatewayv1.RouteConditionAccepted), status.accepted, status.acceptedReason, status.acceptedMessage, route.Generation))
		controller.SetCondition(&parent.Conditions, controller.Condition(string(gatewayv1.RouteConditionResolvedRefs), status.resolved, status.resolvedReason, status.resolvedMessage, route.Generation))
		controller.SetCondition(&parent.Conditions, controller.Condition(config.Domain()+"/Programmed", status.programmed, status.programmedReason, status.programmedMessage, route.Generation))
		return false
	})
}

func applyRouteProviderFailureStatuses(
	config controller.Config,
	route *gatewayv1.HTTPRoute,
	updates []parentStatusUpdate,
	policy controller.ProviderFailurePolicy,
	referencesEvaluated bool,
) bool {
	return mergeRouteParentStatuses(config, route, updates, func(parent *gatewayv1.RouteParentStatus, status routeReconcileStatus) bool {
		resolvedGeneration := controller.ConditionObservedGeneration(
			parent.Conditions,
			string(gatewayv1.RouteConditionResolvedRefs),
			route.Generation,
			referencesEvaluated,
		)
		programmedGeneration := controller.ConditionObservedGeneration(
			parent.Conditions,
			config.Domain()+"/Programmed",
			route.Generation,
			policy.AdvancesObservedGeneration,
		)
		transitioned := controller.ConditionTransitioned(
			parent.Conditions,
			config.Domain()+"/Programmed",
			status.programmed,
			status.programmedReason,
			status.programmedMessage,
			programmedGeneration,
		)
		controller.SetCondition(&parent.Conditions, controller.Condition(string(gatewayv1.RouteConditionAccepted), status.accepted, status.acceptedReason, status.acceptedMessage, route.Generation))
		controller.SetCondition(&parent.Conditions, controller.Condition(string(gatewayv1.RouteConditionResolvedRefs), status.resolved, status.resolvedReason, status.resolvedMessage, resolvedGeneration))
		controller.SetCondition(&parent.Conditions, controller.Condition(config.Domain()+"/Programmed", status.programmed, status.programmedReason, status.programmedMessage, programmedGeneration))
		return transitioned
	})
}

// mergeRouteParentStatuses owns parent retention, deduplication, and ordering.
// The caller computes conditions because normal and failed evaluations advance
// ObservedGeneration and publish Warning events under different rules.
func mergeRouteParentStatuses(
	config controller.Config,
	route *gatewayv1.HTTPRoute,
	updates []parentStatusUpdate,
	apply func(*gatewayv1.RouteParentStatus, routeReconcileStatus) bool,
) bool {
	existing := append([]gatewayv1.RouteParentStatus(nil), route.Status.Parents...)
	parents := make([]gatewayv1.RouteParentStatus, 0, len(existing)+len(updates))
	for _, parent := range existing {
		if retainParentStatus(config, route, parent, updates) {
			parents = append(parents, parent)
		}
	}
	transitioned := false
	for index, update := range updates {
		if parentStatusUpdateAppearedEarlier(route.Namespace, updates, index) {
			continue
		}
		parent := routeParentStatus(route, config.ControllerName, existing, update.parent)
		transitioned = apply(&parent, update.status) || transitioned
		parents = append(parents, parent)
	}
	route.Status.Parents = preserveParentStatusOrder(route.Namespace, existing, parents)
	return transitioned
}

func parentStatusUpdateAppearedEarlier(
	routeNamespace string,
	updates []parentStatusUpdate,
	index int,
) bool {
	for earlier := 0; earlier < index; earlier++ {
		if controller.ParentRefsEqual(
			updates[earlier].parent,
			routeNamespace,
			updates[index].parent,
			routeNamespace,
		) {
			return true
		}
	}
	return false
}

func applyDeletingRouteProviderFailure(
	config controller.Config,
	route *gatewayv1.HTTPRoute,
	parent gatewayv1.ParentReference,
	policy controller.ProviderFailurePolicy,
) bool {
	transitioned := false
	for index := range route.Status.Parents {
		candidate := &route.Status.Parents[index]
		if candidate.ControllerName != config.ControllerName ||
			!controller.SameGatewayTarget(candidate.ParentRef, route.Namespace, parent, route.Namespace) {
			continue
		}
		parentInSpec := false
		for _, ref := range route.Spec.ParentRefs {
			if controller.ParentRefsEqual(candidate.ParentRef, route.Namespace, ref, route.Namespace) {
				parentInSpec = true
				break
			}
		}
		if !parentInSpec {
			continue
		}
		programmedGeneration := controller.ConditionObservedGeneration(
			candidate.Conditions,
			config.Domain()+"/Programmed",
			route.Generation,
			policy.AdvancesObservedGeneration,
		)
		transitioned = controller.ConditionTransitioned(
			candidate.Conditions,
			config.Domain()+"/Programmed",
			policy.ConditionStatus,
			policy.Reason,
			policy.Message,
			programmedGeneration,
		) || transitioned
		controller.SetCondition(&candidate.Conditions, controller.Condition(
			config.Domain()+"/Programmed",
			policy.ConditionStatus,
			policy.Reason,
			policy.Message,
			programmedGeneration,
		))
	}
	return transitioned
}

func routeParentStatus(
	route *gatewayv1.HTTPRoute,
	controllerName gatewayv1.GatewayController,
	existing []gatewayv1.RouteParentStatus,
	parent gatewayv1.ParentReference,
) gatewayv1.RouteParentStatus {
	status := gatewayv1.RouteParentStatus{ParentRef: parent, ControllerName: controllerName}
	for _, candidate := range existing {
		if candidate.ControllerName != controllerName ||
			!controller.ParentRefsEqual(candidate.ParentRef, route.Namespace, parent, route.Namespace) {
			continue
		}
		status.Conditions = append([]metav1.Condition(nil), candidate.Conditions...)
		break
	}
	return status
}

func retainParentStatus(config controller.Config, route *gatewayv1.HTTPRoute, parent gatewayv1.RouteParentStatus, updates []parentStatusUpdate) bool {
	if parent.ControllerName != config.ControllerName {
		return true
	}
	for _, update := range updates {
		if controller.ParentRefsEqual(parent.ParentRef, route.Namespace, update.parent, route.Namespace) {
			return false
		}
	}
	for _, ref := range route.Spec.ParentRefs {
		if controller.ParentRefsEqual(parent.ParentRef, route.Namespace, ref, route.Namespace) {
			return true
		}
	}
	return false
}

func (s *httpRouteScope) expandParentStatuses(updates []parentStatusUpdate) []parentStatusUpdate {
	if len(updates) == 0 || len(s.parents) == 0 {
		return append([]parentStatusUpdate(nil), updates...)
	}
	result := make([]parentStatusUpdate, 0, len(s.parents))
	for _, parent := range s.parents {
		status := updates[0].status
		for _, update := range updates {
			if controller.ParentRefsEqual(update.parent, s.route.Namespace, parent.ref, s.route.Namespace) {
				status = update.status
				break
			}
		}
		if err := s.parentAttachmentError(parent.ref); err != nil {
			status = statusForRouteBuildError(err)
		}
		result = append(result, parentStatusUpdate{parent: parent.ref, status: status})
	}
	return result
}

func (s *httpRouteScope) parentAttachmentError(ref gatewayv1.ParentReference) error {
	for _, attachment := range s.attachments {
		if controller.ParentRefsEqual(attachment.Reference, s.route.Namespace, ref, s.route.Namespace) {
			if attachment.State == model.AttachmentAccepted {
				return nil
			}
			if len(attachment.Diagnostics) > 0 {
				return routeAttachmentError(attachment.Diagnostics[0])
			}
			return newRouteBuildError(routeErrorNoMatchingParent, "no listener accepts the parent reference")
		}
	}
	for _, parent := range s.parents {
		if controller.ParentRefsEqual(parent.ref, s.route.Namespace, ref, s.route.Namespace) {
			return validateRouteParent(s.route, parent)
		}
	}
	return nil
}

// Keep existing entries in place so independently evaluated parents do not
// reorder the list and cause status writes on every successful resync.
func preserveParentStatusOrder(namespace string, existing, desired []gatewayv1.RouteParentStatus) []gatewayv1.RouteParentStatus {
	result := make([]gatewayv1.RouteParentStatus, 0, len(desired))
	used := make([]bool, len(desired))
	for _, previous := range existing {
		for i, current := range desired {
			if !used[i] && previous.ControllerName == current.ControllerName && controller.ParentRefsEqual(previous.ParentRef, namespace, current.ParentRef, namespace) {
				result = append(result, current)
				used[i] = true
				break
			}
		}
	}
	for i, current := range desired {
		if !used[i] {
			result = append(result, current)
		}
	}
	return result
}
