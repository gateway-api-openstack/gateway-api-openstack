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
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	gatewaycontroller "github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/gateway"
)

type routeSnapshotPurpose int

const (
	routeSnapshotForEnsure routeSnapshotPurpose = iota
	routeSnapshotForBinding
)

type routeValidationDisposition int

const (
	routeValidationUnknown routeValidationDisposition = iota
	routeValidationGatewayPending
	routeValidationBindingRequired
	routeValidationReady
)

type routeBindingRequirement int

const (
	routeBindingOptional routeBindingRequirement = iota
	routeBindingRequired
)

type routeBindingSnapshot struct {
	identity       cloud.Identity
	present        bool
	hasFinalizer   bool
	matchesDesired bool
}

type routeSelectionSnapshot struct {
	slot     routeSlotDecision
	selected bool
}

// routeValidationSnapshot contains one live view of every Kubernetes input
// that authorizes a route graph mutation. It is local to one mutation attempt
// and must not be retained after releasing the Gateway graph lock.
type routeValidationSnapshot struct {
	route             *gatewayv1.HTTPRoute
	gateway           *gatewayv1.Gateway
	gatewayClass      *gatewayv1.GatewayClass
	binding           routeBindingSnapshot
	selection         routeSelectionSnapshot
	desired           cloud.RouteSpec
	backends          *routeModelInputs
	gatewayProgrammed bool
	disposition       routeValidationDisposition
}

type routeCleanupDisposition int

const (
	routeCleanupUnknown routeCleanupDisposition = iota
	routeCleanupRequired
	routeCleanupStillDesired
)

type routeCleanupSnapshot struct {
	route       *gatewayv1.HTTPRoute
	identity    cloud.Identity
	disposition routeCleanupDisposition
}

type routeParentObservation struct {
	ref          gatewayv1.ParentReference
	gateway      *gatewayv1.Gateway
	gatewayClass *gatewayv1.GatewayClass
	managed      bool
}

func (r *Reconciler) buildRouteValidationSnapshot(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
	purpose routeSnapshotPurpose,
) (routeValidationSnapshot, error) {
	return r.buildRouteValidationSnapshotFromInputs(ctx, expectedRoute, expectedGateway, purpose, nil)
}

func (r *Reconciler) buildRouteValidationSnapshotFromInputs(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
	purpose routeSnapshotPurpose,
	inputs *routeModelInputs,
) (routeValidationSnapshot, error) {
	snapshot, parent, err := r.observeRouteValidationSnapshot(ctx, expectedRoute, expectedGateway, purpose)
	if err != nil || snapshot.disposition == routeValidationGatewayPending {
		return snapshot, err
	}
	return r.completeRouteValidationSnapshot(ctx, snapshot, parent, purpose, inputs)
}

func (r *Reconciler) observeRouteValidationSnapshot(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
	purpose routeSnapshotPurpose,
) (routeValidationSnapshot, managedRouteParent, error) {
	snapshot, err := r.observeRouteValidationInputs(ctx, expectedRoute, expectedGateway)
	if err != nil {
		return routeValidationSnapshot{}, managedRouteParent{}, err
	}
	parent, err := r.validateRouteSnapshotParent(&snapshot, purpose)
	if err != nil || snapshot.disposition == routeValidationGatewayPending {
		return snapshot, parent, err
	}
	if purpose == routeSnapshotForBinding && !snapshot.gatewayProgrammed {
		return routeValidationSnapshot{}, managedRouteParent{}, errHTTPRouteChanged
	}
	return snapshot, parent, nil
}

func (r *Reconciler) completeRouteValidationSnapshot(
	ctx context.Context,
	snapshot routeValidationSnapshot,
	parent managedRouteParent,
	purpose routeSnapshotPurpose,
	inputs *routeModelInputs,
) (routeValidationSnapshot, error) {
	if inputs == nil {
		routes, err := r.liveGraphRoutes(ctx, controller.GatewayIdentity(r.Config, snapshot.gateway))
		if err != nil {
			return routeValidationSnapshot{}, err
		}
		inputs, err = collectRouteModelInputs(ctx, r.APIReader, snapshot.gateway, routes)
		if err != nil {
			return routeValidationSnapshot{}, err
		}
	}
	selection, desired, err := r.selectAndBuildRouteModel(ctx, snapshot, parent, purpose, inputs)
	if err != nil {
		return routeValidationSnapshot{}, err
	}
	snapshot.selection = selection
	snapshot.desired = desired
	snapshot.backends = inputs

	if err := r.finishRouteBindingSnapshot(&snapshot, purpose); err != nil {
		return routeValidationSnapshot{}, err
	}
	return snapshot, nil
}

func (r *Reconciler) observeRouteValidationInputs(
	ctx context.Context,
	expectedRoute *gatewayv1.HTTPRoute,
	expectedGateway *gatewayv1.Gateway,
) (routeValidationSnapshot, error) {
	if r.APIReader == nil {
		return routeValidationSnapshot{}, controller.ErrAPIReaderRequired
	}

	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expectedRoute), &route); err != nil {
		return routeValidationSnapshot{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		expectedRoute.UID,
		expectedRoute.Generation,
		!expectedRoute.DeletionTimestamp.IsZero(),
	) {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expectedGateway), &gateway); err != nil {
		return routeValidationSnapshot{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expectedGateway.UID || gateway.Generation != expectedGateway.Generation ||
		!gateway.DeletionTimestamp.IsZero() {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}

	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		return routeValidationSnapshot{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return routeValidationSnapshot{}, controller.ErrUnsupportedGatewayAPIVersion
	}
	if !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) ||
		controller.ValidateGatewayBinding(r.Config, &gateway) != nil {
		return routeValidationSnapshot{}, errHTTPRouteChanged
	}
	return routeValidationSnapshot{
		route:        &route,
		gateway:      &gateway,
		gatewayClass: &gatewayClass,
		disposition:  routeValidationReady,
	}, nil
}

func (r *Reconciler) validateRouteSnapshotParent(
	snapshot *routeValidationSnapshot,
	purpose routeSnapshotPurpose,
) (managedRouteParent, error) {
	if _, err := gatewaycontroller.Validate(snapshot.gateway); err != nil {
		if purpose == routeSnapshotForBinding {
			return managedRouteParent{}, errHTTPRouteChanged
		}
		snapshot.disposition = routeValidationGatewayPending
		return managedRouteParent{}, nil
	}
	var parent managedRouteParent
	found := false
	for _, ref := range snapshot.route.Spec.ParentRefs {
		if !parentTargetsGateway(ref, snapshot.route.Namespace, snapshot.gateway) {
			continue
		}
		candidate := managedRouteParent{ref: ref, gateway: snapshot.gateway, gatewayClass: snapshot.gatewayClass}
		if err := validateRouteParent(snapshot.route, candidate); err != nil {
			continue
		}
		parent, found = candidate, true
		break
	}
	if !found {
		return managedRouteParent{}, routeSnapshotRejection(purpose, newRouteBuildError(routeErrorNoMatchingParent, "no parentRef selects an eligible listener"))
	}
	snapshot.gatewayProgrammed = gatewayProgrammedForGeneration(snapshot.gateway)
	return parent, nil
}

func (r *Reconciler) selectAndBuildRouteModel(
	ctx context.Context,
	snapshot routeValidationSnapshot,
	parent managedRouteParent,
	purpose routeSnapshotPurpose,
	inputs *routeModelInputs,
) (routeSelectionSnapshot, cloud.RouteSpec, error) {
	if err := validateRuntimeRouteProfile(snapshot.route); err != nil {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotReadError(purpose, err)
	}
	structurallySupported := structurallySupportedRoute(snapshot.route)
	if !structurallySupported && purpose == routeSnapshotForBinding {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, errHTTPRouteChanged
	}
	topology, err := controller.StoredGatewayTopology(r.Config, snapshot.gateway)
	if err != nil {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, err
	}
	evaluate := func(route *gatewayv1.HTTPRoute) (routeSlotDecision, error) {
		for _, candidate := range inputs.logical.Routes {
			if candidate.Source.UID == route.UID && !hasAcceptedAttachment(candidate.Parents) {
				return routeSlotDecision{rejection: newRouteBuildError(routeErrorNotAllowed, "no listener accepts the Route")}, nil
			}
		}
		reference := route.Spec.Rules[0].BackendRefs[0]
		namespace := route.Namespace
		if reference.Namespace != nil {
			namespace = string(*reference.Namespace)
		}
		service := inputs.service(types.NamespacedName{Namespace: namespace, Name: string(reference.Name)})
		return routeSlotForService(service, *reference.Port, topology.Topology.MemberMode), nil
	}
	selection := routeSelectionSnapshot{}
	if structurallySupported {
		decision, _ := evaluate(snapshot.route)
		selection.slot = decision
		if !decision.canReserve {
			return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotRejection(purpose, decision.rejection)
		}
	}
	selected, err := selectRouteCandidate(snapshot.route, parent, inputs.input.Routes, evaluate)
	if err != nil {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotReadError(purpose, err)
	}
	selection.selected = selected
	if !selected {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotRejection(purpose, newRouteBuildError(
			routeErrorUnsupported,
			"only one HTTPRoute per Gateway is supported. An older route is already selected",
		))
	}

	desired, err := r.buildRouteModelFromInputs(ctx, r.APIReader, snapshot.route, snapshot.gateway, inputs)
	if err != nil {
		return routeSelectionSnapshot{}, cloud.RouteSpec{}, routeSnapshotReadError(purpose, err)
	}
	return selection, desired, nil
}

func (r *Reconciler) finishRouteBindingSnapshot(
	snapshot *routeValidationSnapshot,
	purpose routeSnapshotPurpose,
) error {
	storedBinding, bindingPresent, err := controller.StoredRouteBinding(r.Config, snapshot.route, string(snapshot.gateway.UID))
	storedIdentity := storedBinding.Identity
	if storedBinding.Lifecycle == controller.RouteDetaching {
		return errHTTPRouteChanged
	}
	if err != nil {
		return err
	}
	desiredIdentity := controller.RouteIdentity(r.Config, snapshot.gateway, snapshot.route)
	binding := routeBindingSnapshot{
		identity:       storedIdentity,
		present:        bindingPresent,
		hasFinalizer:   controllerutil.ContainsFinalizer(snapshot.route, r.Config.RouteFinalizer()),
		matchesDesired: bindingPresent && storedIdentity == desiredIdentity,
	}
	snapshot.binding = binding

	if purpose == routeSnapshotForBinding {
		return r.validateRouteBindingTransition(*snapshot)
	}

	boundToCurrentGateway := binding.matchesDesired && binding.hasFinalizer
	if !snapshot.gatewayProgrammed && !boundToCurrentGateway {
		snapshot.disposition = routeValidationGatewayPending
		return nil
	}
	desiredBinding := snapshot.route.DeepCopy()
	if err := r.applyRouteBinding(desiredBinding, snapshot.gateway); err != nil {
		return err
	}
	if !routeBindingMetadataEqual(snapshot.route, desiredBinding) {
		snapshot.disposition = routeValidationBindingRequired
	}
	return nil
}

// validateRouteBindingTransition prevents a binding checkpoint from adopting
// incomplete metadata or overwriting cleanup authority for another Gateway.
// It is pure so an ensure snapshot can be reused without rebuilding the
// backend model.
func (r *Reconciler) validateRouteBindingTransition(snapshot routeValidationSnapshot) error {
	if !snapshot.gatewayProgrammed {
		return errHTTPRouteChanged
	}
	desiredIdentity := controller.RouteIdentity(r.Config, snapshot.gateway, snapshot.route)
	if snapshot.binding.present && !controller.SameGatewayIdentity(snapshot.binding.identity, desiredIdentity) {
		return errHTTPRouteChanged
	}
	return nil
}

func gatewayProgrammedForGeneration(gateway *gatewayv1.Gateway) bool {
	programmed := meta.FindStatusCondition(
		gateway.Status.Conditions,
		string(gatewayv1.GatewayConditionProgrammed),
	)
	return programmed != nil && programmed.Status == metav1.ConditionTrue &&
		programmed.ObservedGeneration == gateway.Generation
}

func routeSnapshotReadError(purpose routeSnapshotPurpose, err error) error {
	if purpose == routeSnapshotForBinding {
		return errors.Join(errHTTPRouteChanged, err)
	}
	return err
}

func routeSnapshotRejection(purpose routeSnapshotPurpose, rejection error) error {
	if purpose == routeSnapshotForBinding {
		return errHTTPRouteChanged
	}
	return rejection
}

// sealRouteMutationSnapshot verifies that the ownership inputs used to build
// a desired graph have not changed before the first Kubernetes or OpenStack
// mutation. The repeated live reads are a mutation fence, not a second model
// build.
func (r *Reconciler) sealRouteMutationSnapshot(
	ctx context.Context,
	snapshot routeValidationSnapshot,
	bindingRequirement routeBindingRequirement,
) error {
	if err := r.sealRouteOwnershipSnapshot(ctx, snapshot, bindingRequirement); err != nil {
		return err
	}
	routes, err := r.liveGraphRoutes(ctx, controller.GatewayIdentity(r.Config, snapshot.gateway))
	if err != nil {
		return err
	}
	if !slices.Equal(routeGraphRevisions(snapshot.backends.input.Routes), routeGraphRevisions(routes)) {
		return errHTTPRouteChanged
	}
	return snapshot.backends.validate(ctx, r.APIReader)
}

// sealRouteOwnershipSnapshot checks durable route and Gateway authority. The
// graph writer validates shared backend inputs once after all route fences.
func (r *Reconciler) sealRouteOwnershipSnapshot(ctx context.Context, snapshot routeValidationSnapshot, bindingRequirement routeBindingRequirement) error {
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return err
	}
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot.route), &route); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		snapshot.route.UID,
		snapshot.route.Generation,
		!snapshot.route.DeletionTimestamp.IsZero(),
	) {
		return errHTTPRouteChanged
	}
	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot.gateway), &gateway); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != snapshot.gateway.UID || gateway.Generation != snapshot.gateway.Generation ||
		!gateway.DeletionTimestamp.IsZero() {
		return errHTTPRouteChanged
	}
	if !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) ||
		!controller.GatewayBindingMetadataEqual(r.Config, &gateway, snapshot.gateway) ||
		controller.ValidateGatewayBinding(r.Config, &gateway) != nil {
		return errHTTPRouteChanged
	}
	currentBinding, currentPresent, err := controller.StoredRouteBinding(r.Config, &route, string(gateway.UID))
	if err != nil {
		return err
	}
	previousBinding, previousPresent, err := controller.StoredRouteBinding(r.Config, snapshot.route, string(gateway.UID))
	if err != nil {
		return err
	}
	if currentPresent != previousPresent || currentBinding != previousBinding {
		return errHTTPRouteChanged
	}
	if bindingRequirement == routeBindingRequired {
		stored, present, err := r.storedRouteIdentity(&route, string(gateway.UID))
		if err != nil {
			return err
		}
		if !present || stored != controller.RouteIdentity(r.Config, &gateway, &route) ||
			!controllerutil.ContainsFinalizer(&route, r.Config.RouteFinalizer()) {
			return errHTTPRouteChanged
		}
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		return errHTTPRouteChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return controller.ErrUnsupportedGatewayAPIVersion
	}

	return nil
}

func (r *Reconciler) buildRouteCleanupSnapshot(
	ctx context.Context,
	expected *gatewayv1.HTTPRoute,
	expectedIdentity cloud.Identity,
) (routeCleanupSnapshot, error) {
	if r.APIReader == nil {
		return routeCleanupSnapshot{}, controller.ErrAPIReaderRequired
	}
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &route); err != nil {
		return routeCleanupSnapshot{}, errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		expected.UID,
		expected.Generation,
		!expected.DeletionTimestamp.IsZero(),
	) || route.ResourceVersion != expected.ResourceVersion {
		return routeCleanupSnapshot{}, errHTTPRouteChanged
	}
	stored, present, err := r.storedRouteIdentity(&route, expectedIdentity.GatewayUID)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	if !present || stored != expectedIdentity {
		return routeCleanupSnapshot{}, errHTTPRouteChanged
	}
	snapshot := routeCleanupSnapshot{
		route:       &route,
		identity:    stored,
		disposition: routeCleanupRequired,
	}
	binding, _, _ := controller.StoredRouteBinding(r.Config, &route, expectedIdentity.GatewayUID)
	if !route.DeletionTimestamp.IsZero() || binding.Lifecycle == controller.RouteDetaching {
		return snapshot, nil
	}

	parentObservations, err := r.observeRouteParentsForCleanup(ctx, &route)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	if routeCleanupAllowedDuringVersionMismatch(parentObservations, stored) {
		return snapshot, nil
	}
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return routeCleanupSnapshot{}, err
	}
	parentObservations, err = r.observeRouteParentsForCleanup(ctx, &route)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	managedParents := managedParentsFromObservations(parentObservations)
	var parent managedRouteParent
	found := false
	for _, candidate := range managedParents {
		if string(candidate.gateway.UID) == stored.GatewayUID && validateRouteParent(&route, candidate) == nil {
			parent, found = candidate, true
			break
		}
	}
	if !found {
		return snapshot, nil
	}
	if !controller.GatewayClassSupportsInstalledVersion(parent.gatewayClass) {
		return routeCleanupSnapshot{}, controller.ErrUnsupportedGatewayAPIVersion
	}
	if validateRouteParent(&route, parent) != nil || !structurallySupportedRoute(&route) {
		return snapshot, nil
	}
	decision, err := r.evaluateRouteSlotWithReader(ctx, r.APIReader, &route, parent.gateway)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	if !decision.canReserve {
		return snapshot, nil
	}
	selected, err := r.isSelectedRouteWithReader(ctx, r.APIReader, &route, parent, false)
	if err != nil {
		return routeCleanupSnapshot{}, err
	}
	if !selected {
		return snapshot, nil
	}
	desired, err := r.buildRouteModelWithReader(ctx, r.APIReader, &route, parent.gateway)
	if err != nil {
		var buildError *routeBuildError
		if errors.As(err, &buildError) || apierrors.IsNotFound(err) {
			return snapshot, nil
		}
		return routeCleanupSnapshot{}, err
	}
	if desired.Identity == stored {
		snapshot.disposition = routeCleanupStillDesired
	}
	return snapshot, nil
}

func (r *Reconciler) observeRouteParentsForCleanup(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
) ([]routeParentObservation, error) {
	parents := make([]routeParentObservation, 0, len(route.Spec.ParentRefs))
	for _, parentRef := range route.Spec.ParentRefs {
		if !controller.IsGatewayParentRef(parentRef) {
			continue
		}
		observation := routeParentObservation{ref: parentRef}
		namespace := route.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}
		var gateway gatewayv1.Gateway
		if err := r.APIReader.Get(
			ctx,
			types.NamespacedName{Namespace: namespace, Name: string(parentRef.Name)},
			&gateway,
		); err != nil {
			if apierrors.IsNotFound(err) {
				parents = append(parents, observation)
				continue
			}
			return nil, fmt.Errorf("get parent Gateway during Gateway API version check: %w", err)
		}
		observation.gateway = &gateway
		var gatewayClass gatewayv1.GatewayClass
		if err := r.APIReader.Get(
			ctx,
			types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
			&gatewayClass,
		); err != nil {
			if apierrors.IsNotFound(err) {
				parents = append(parents, observation)
				continue
			}
			return nil, fmt.Errorf("get parent GatewayClass during Gateway API version check: %w", err)
		}
		observation.gatewayClass = &gatewayClass
		if gatewayClass.Spec.ControllerName == r.Config.ControllerName {
			observation.managed = true
		}
		parents = append(parents, observation)
	}
	return parents, nil
}

func routeCleanupAllowedDuringVersionMismatch(parents []routeParentObservation, stored cloud.Identity) bool {
	for _, parent := range parents {
		if parent.gateway == nil || parent.gateway.Namespace != stored.GatewayNamespace || parent.gateway.Name != stored.GatewayName || string(parent.gateway.UID) != stored.GatewayUID {
			continue
		}
		if parent.gatewayClass == nil {
			return false
		}
		if parent.managed && parent.gateway.DeletionTimestamp.IsZero() {
			return false
		}
	}
	return true
}

func managedParentsFromObservations(observations []routeParentObservation) []managedRouteParent {
	parents := make([]managedRouteParent, 0, len(observations))
	for _, observation := range observations {
		if !observation.managed {
			continue
		}
		parents = append(parents, managedRouteParent{
			ref:          observation.ref,
			gateway:      observation.gateway,
			gatewayClass: observation.gatewayClass,
		})
	}
	return parents
}

func (r *Reconciler) sealRouteCleanupSnapshot(
	ctx context.Context,
	snapshot routeCleanupSnapshot,
) error {
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot.route), &route); err != nil {
		return errors.Join(errHTTPRouteChanged, client.IgnoreNotFound(err))
	}
	if !sameHTTPRouteRevision(
		&route,
		snapshot.route.UID,
		snapshot.route.Generation,
		!snapshot.route.DeletionTimestamp.IsZero(),
	) || route.ResourceVersion != snapshot.route.ResourceVersion {
		return errHTTPRouteChanged
	}
	stored, present, err := r.storedRouteIdentity(&route, snapshot.identity.GatewayUID)
	if err != nil {
		return err
	}
	if !present || stored != snapshot.identity {
		return errHTTPRouteChanged
	}
	return nil
}
