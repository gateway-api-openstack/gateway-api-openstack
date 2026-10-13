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
	"reflect"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
)

type gatewayBindingRequirement int

const (
	gatewayBindingOptional gatewayBindingRequirement = iota
	gatewayBindingRequired
)

// gatewayMutationSnapshot contains the live Kubernetes inputs that authorize
// one Gateway metadata or provider mutation. It is request-local and must not
// be retained across reconciliations.
type gatewayMutationSnapshot struct {
	gateway      *gatewayv1.Gateway
	gatewayClass *gatewayv1.GatewayClass
	listener     *gatewayv1.Listener
	binding      gatewayBindingRequirement
	topology     classconfig.Resolved
}

// observeGatewayMutationSnapshot builds the model used by one mutation
// attempt. sealGatewayMutationSnapshot must still run after model construction
// so a controller handoff or binding change cannot cross the mutation fence.
func (r *Reconciler) observeGatewayMutationSnapshot(
	ctx context.Context,
	expected *gatewayv1.Gateway,
	binding gatewayBindingRequirement,
	expectedListenerPort string,
) (*gatewayMutationSnapshot, error) {
	if r.APIReader == nil {
		return nil, controller.ErrAPIReaderRequired
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &gateway); err != nil {
		return nil, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expected.UID || gateway.Generation != expected.Generation ||
		!gateway.DeletionTimestamp.IsZero() {
		return nil, errGatewayChanged
	}

	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		return nil, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		return nil, errGatewayChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return nil, controller.ErrUnsupportedGatewayAPIVersion
	}
	if binding == gatewayBindingRequired && !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) {
		return nil, errGatewayChanged
	}
	if err := controller.ValidateGatewayBinding(r.Config, &gateway); err != nil {
		return nil, errors.Join(errGatewayChanged, err)
	}

	listener, validationErr := Validate(&gateway)
	if validationErr != nil {
		return nil, errGatewayChanged
	}
	listenerPort := strconv.Itoa(int(listener.Port))
	if expectedListenerPort != "" && listenerPort != expectedListenerPort {
		return nil, errGatewayChanged
	}
	if binding == gatewayBindingRequired &&
		gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] != listenerPort {
		return nil, errGatewayChanged
	}

	var topology classconfig.Resolved
	var err error
	if controller.HasGatewayTopologyBinding(r.Config, &gateway) {
		topology, err = controller.StoredGatewayTopology(r.Config, &gateway)
	} else if binding == gatewayBindingRequired {
		return nil, fmt.Errorf("%w: Gateway topology binding is required", cloud.ErrOwnershipConflict)
	} else {
		topology, err = classconfig.Resolve(ctx, r.APIReader, r.TopologyResolver, &gatewayClass)
	}
	if err != nil {
		return nil, err
	}
	return &gatewayMutationSnapshot{
		gateway:      &gateway,
		topology:     topology,
		gatewayClass: &gatewayClass,
		listener:     listener,
		binding:      binding,
	}, nil
}

// sealGatewayMutationSnapshot rechecks the small ownership fingerprint used by
// the observed model immediately before mutation. The second read is a safety
// fence, not a cache refresh that callers may omit.
func (r *Reconciler) sealGatewayMutationSnapshot(
	ctx context.Context,
	snapshot *gatewayMutationSnapshot,
) error {
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return err
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot.gateway), &gateway); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != snapshot.gateway.UID || gateway.Generation != snapshot.gateway.Generation ||
		!gateway.DeletionTimestamp.IsZero() ||
		!controller.GatewayBindingMetadataEqual(r.Config, &gateway, snapshot.gateway) {
		return errGatewayChanged
	}
	if snapshot.binding == gatewayBindingRequired {
		if !controllerutil.ContainsFinalizer(&gateway, r.Config.GatewayFinalizer()) ||
			gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] == "" ||
			gateway.Annotations[r.Config.GatewayClusterIDAnnotation()] == "" ||
			gateway.Annotations[r.Config.GatewayProjectIDAnnotation()] == "" ||
			controller.ValidateGatewayBinding(r.Config, &gateway) != nil {
			return errGatewayChanged
		}
	}

	if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(snapshot.gatewayClass.Name) {
		return errGatewayChanged
	}
	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: snapshot.gatewayClass.Name},
		&gatewayClass,
	); err != nil {
		return errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		return errGatewayChanged
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return controller.ErrUnsupportedGatewayAPIVersion
	}
	if snapshot.binding == gatewayBindingOptional && !controller.HasGatewayTopologyBinding(r.Config, snapshot.gateway) {
		if !reflect.DeepEqual(gatewayClass.Spec.ParametersRef, snapshot.gatewayClass.Spec.ParametersRef) {
			return errGatewayChanged
		}
		var parameters configv1alpha1.OpenStackGatewayClassConfig
		if err := r.APIReader.Get(ctx, types.NamespacedName{Name: snapshot.topology.ParametersName}, &parameters); err != nil {
			return err
		}
		if parameters.UID != snapshot.topology.ParametersUID || parameters.Generation != snapshot.topology.ParametersGeneration || !parameters.DeletionTimestamp.IsZero() {
			return errGatewayChanged
		}
	}
	return nil
}

type gatewayReconcilePhase int

const (
	gatewayPhaseIgnore gatewayReconcilePhase = iota
	gatewayPhaseCleanupUnmanaged
	gatewayPhaseUnsupportedVersion
	gatewayPhaseInvalidBinding
	gatewayPhaseInvalidSpec
	gatewayPhaseReplaceGraph
	gatewayPhaseBind
	gatewayPhaseEnsure
)

// gatewayPhaseDecision is a pure classification of the cached inputs for one
// normal reconciliation. Mutation helpers build their own live snapshots and
// do not treat this decision as mutation authority.
type gatewayPhaseDecision struct {
	phase        gatewayReconcilePhase
	listener     *gatewayv1.Listener
	listenerPort string
	validation   error
}

func (r *Reconciler) decideGatewayPhase(scope *gatewayScope) gatewayPhaseDecision {
	gateway := scope.gateway
	if !scope.managed {
		if controller.GatewayHasControllerBinding(r.Config, gateway) {
			return gatewayPhaseDecision{phase: gatewayPhaseCleanupUnmanaged}
		}
		return gatewayPhaseDecision{phase: gatewayPhaseIgnore}
	}
	if scope.gatewayClass != nil && !controller.GatewayClassSupportsInstalledVersion(scope.gatewayClass) {
		return gatewayPhaseDecision{phase: gatewayPhaseUnsupportedVersion}
	}
	if err := controller.ValidateGatewayBinding(r.Config, gateway); err != nil {
		return gatewayPhaseDecision{phase: gatewayPhaseInvalidBinding, validation: err}
	}

	listener, err := Validate(gateway)
	if err != nil {
		return gatewayPhaseDecision{phase: gatewayPhaseInvalidSpec, validation: err}
	}
	listenerPort := strconv.Itoa(int(listener.Port))
	storedListenerPort := gateway.Annotations[r.Config.GatewayListenerPortAnnotation()]
	if storedListenerPort != "" && storedListenerPort != listenerPort {
		return gatewayPhaseDecision{
			phase: gatewayPhaseReplaceGraph, listener: listener, listenerPort: listenerPort,
		}
	}
	if !r.gatewayBindingComplete(gateway) {
		return gatewayPhaseDecision{phase: gatewayPhaseBind, listener: listener, listenerPort: listenerPort}
	}
	return gatewayPhaseDecision{phase: gatewayPhaseEnsure, listener: listener, listenerPort: listenerPort}
}

func (r *Reconciler) gatewayBindingComplete(gateway *gatewayv1.Gateway) bool {
	return controllerutil.ContainsFinalizer(gateway, r.Config.GatewayFinalizer()) &&
		gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] != "" &&
		gateway.Annotations[r.Config.GatewayClusterIDAnnotation()] != "" &&
		gateway.Annotations[r.Config.GatewayProjectIDAnnotation()] != ""
}

type gatewayCleanupDisposition string

const (
	gatewayCleanupDelete  gatewayCleanupDisposition = "Delete"
	gatewayCleanupRebuild gatewayCleanupDisposition = "Rebuild"
	gatewayCleanupHold    gatewayCleanupDisposition = "Hold"
)

// gatewayCleanupSnapshot records the live binding and the cleanup authority
// derived from the current Gateway and GatewayClass lifecycle.
type gatewayCleanupSnapshot struct {
	identity     cloud.Identity
	disposition  gatewayCleanupDisposition
	listenerPort string
}

func (r *Reconciler) observeGatewayCleanupSnapshot(
	ctx context.Context,
	expected *gatewayv1.Gateway,
) (*gatewayCleanupSnapshot, error) {
	if r.APIReader == nil {
		return nil, controller.ErrAPIReaderRequired
	}

	var gateway gatewayv1.Gateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), &gateway); err != nil {
		return nil, errors.Join(errGatewayChanged, client.IgnoreNotFound(err))
	}
	if gateway.UID != expected.UID || gateway.Generation != expected.Generation ||
		gateway.ResourceVersion != expected.ResourceVersion ||
		gateway.DeletionTimestamp.IsZero() != expected.DeletionTimestamp.IsZero() {
		return nil, errGatewayChanged
	}
	if err := controller.ValidateGatewayBinding(r.Config, &gateway); err != nil {
		return nil, errors.Join(errGatewayChanged, err)
	}

	snapshot := &gatewayCleanupSnapshot{
		identity:    controller.StoredGatewayIdentity(r.Config, &gateway),
		disposition: gatewayCleanupDelete,
	}
	if !gateway.DeletionTimestamp.IsZero() {
		return snapshot, nil
	}

	var gatewayClass gatewayv1.GatewayClass
	if err := r.APIReader.Get(
		ctx,
		types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
		&gatewayClass,
	); err != nil {
		if apierrors.IsNotFound(err) {
			if versionErr := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); versionErr != nil {
				return nil, versionErr
			}
			return snapshot, nil
		}
		return nil, fmt.Errorf("get GatewayClass before Gateway cleanup: %w", err)
	}
	if gatewayClass.Spec.ControllerName != r.Config.ControllerName {
		return snapshot, nil
	}
	if !controller.GatewayClassSupportsInstalledVersion(&gatewayClass) {
		return nil, controller.ErrUnsupportedGatewayAPIVersion
	}
	if err := controller.ValidateInstalledGatewayAPIVersion(ctx, r.APIReader); err != nil {
		return nil, err
	}
	listener, validationErr := Validate(&gateway)
	if validationErr != nil {
		snapshot.disposition = gatewayCleanupRebuild
		return snapshot, nil
	}
	storedListenerPort := gateway.Annotations[r.Config.GatewayListenerPortAnnotation()]
	if storedListenerPort != "" && storedListenerPort != strconv.Itoa(int(listener.Port)) {
		snapshot.disposition = gatewayCleanupRebuild
		snapshot.listenerPort = strconv.Itoa(int(listener.Port))
		return snapshot, nil
	}

	snapshot.disposition = gatewayCleanupHold
	return snapshot, nil
}
