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

package gatewayclass

import (
	"context"
	"errors"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
)

// Reconciler publishes the capabilities of GatewayClasses assigned
// to this controller and prevents a class from being removed while Gateways
// still reference it.
type Reconciler struct {
	// Client reads and patches cached Kubernetes objects.
	client.Client
	// APIReader performs live Gateway API CRD metadata reads.
	APIReader client.Reader
	// Config contains immutable controller configuration.
	Config controller.Config
	// TopologyResolver verifies class infrastructure with read-only cloud APIs.
	TopologyResolver cloud.TopologyResolver
	// Recorder publishes stable diagnostics for cloud authorization failures.
	Recorder events.EventRecorder
}

// Reconcile evaluates one GatewayClass and publishes the status owned by this
// controller.
// +kubebuilder:rbac:groups=gateway-api-openstack.github.io,resources=openstackgatewayclassconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
func (r *Reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (result ctrl.Result, retErr error) {
	if r.APIReader == nil {
		return ctrl.Result{}, controller.ErrAPIReaderRequired
	}
	logger := log.FromContext(ctx).WithValues("gatewayClass", req.Name)
	ctx = log.IntoContext(ctx, logger)
	logger.V(4).Info("Reconciling GatewayClass")

	scope, responsible, err := r.newGatewayClassScope(ctx, req)
	if err != nil || !responsible {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := r.patchGatewayClass(ctx, scope); err != nil {
			result = ctrl.Result{}
			retErr = errors.Join(retErr, err)
		}
	}()

	if !scope.gatewayClass.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, scope)
	}
	return r.reconcileNormal(ctx, scope)
}

func (r *Reconciler) reconcileDelete(
	ctx context.Context,
	scope *gatewayClassScope,
) (ctrl.Result, error) {
	hasGateways, err := r.reconcileGatewayClassReferences(ctx, scope)
	if err != nil {
		return ctrl.Result{}, err
	}
	if scope.finalizerChanged() {
		return ctrl.Result{}, nil
	}
	if !hasGateways {
		return ctrl.Result{}, nil
	}
	return r.reconcileGatewayClassStatus(ctx, scope)
}

func (r *Reconciler) reconcileNormal(
	ctx context.Context,
	scope *gatewayClassScope,
) (ctrl.Result, error) {
	if _, err := r.reconcileGatewayClassReferences(ctx, scope); err != nil {
		return ctrl.Result{}, err
	}
	if scope.finalizerChanged() {
		return ctrl.Result{}, nil
	}
	return r.reconcileGatewayClassStatus(ctx, scope)
}

func (r *Reconciler) reconcileGatewayClassReferences(
	ctx context.Context,
	scope *gatewayClassScope,
) (bool, error) {
	hasGateways, err := r.hasReferencingGateways(ctx, scope.gatewayClass.Name)
	if err != nil {
		return false, fmt.Errorf("list Gateways referencing GatewayClass: %w", err)
	}
	if hasGateways {
		controllerutil.AddFinalizer(scope.gatewayClass, gatewayv1.GatewayClassFinalizerGatewaysExist)
	} else {
		controllerutil.RemoveFinalizer(scope.gatewayClass, gatewayv1.GatewayClassFinalizerGatewaysExist)
	}
	return hasGateways, nil
}

func (r *Reconciler) reconcileGatewayClassStatus(
	ctx context.Context,
	scope *gatewayClassScope,
) (ctrl.Result, error) {
	gatewayClass := scope.gatewayClass
	versionObservation, err := controller.ObserveGatewayAPIVersion(ctx, r.APIReader)
	if err != nil {
		return ctrl.Result{}, err
	}
	if versionObservation.Supported {
		controller.SetCondition(&gatewayClass.Status.Conditions, controller.Condition(
			string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
			metav1.ConditionTrue,
			string(gatewayv1.GatewayClassReasonSupportedVersion),
			versionObservation.Message,
			gatewayClass.Generation,
		))
	} else {
		gatewayClass.Status.SupportedFeatures = nil
		controller.SetCondition(&gatewayClass.Status.Conditions, controller.Condition(
			string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
			metav1.ConditionFalse,
			string(gatewayv1.GatewayClassReasonUnsupportedVersion),
			versionObservation.Message,
			gatewayClass.Generation,
		))
		controller.SetCondition(&gatewayClass.Status.Conditions, controller.Condition(
			string(gatewayv1.GatewayClassConditionStatusAccepted),
			metav1.ConditionFalse,
			string(gatewayv1.GatewayClassReasonUnsupportedVersion),
			versionObservation.Message,
			gatewayClass.Generation,
		))
	}
	if versionObservation.Supported {
		return r.reconcileParameters(ctx, scope)
	}

	result := ctrl.Result{}
	if !versionObservation.Supported {
		result.RequeueAfter = controller.GatewayAPIVersionRequeueAfter(gatewayClass.UID)
	}
	return result, nil
}

func (r *Reconciler) hasReferencingGateways(ctx context.Context, className string) (bool, error) {
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.MatchingFields{controller.IndexGatewayByClass: className}); err != nil {
		return false, err
	}
	return len(gateways.Items) != 0, nil
}

func (r *Reconciler) reconcileParameters(ctx context.Context, scope *gatewayClassScope) (ctrl.Result, error) {
	class := scope.gatewayClass
	// Read the class and its parameters live so a delayed cache event cannot
	// accept an obsolete reference or a class now assigned to another controller.
	live := &gatewayv1.GatewayClass{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(class), live); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if live.UID != class.UID || live.ResourceVersion != class.ResourceVersion || live.Spec.ControllerName != r.Config.ControllerName {
		scope.gatewayClass.Status = scope.original.Status
		return ctrl.Result{Requeue: true}, nil
	}
	_, err := classconfig.Resolve(ctx, r.APIReader, r.TopologyResolver, live)
	class.Status.SupportedFeatures = nil
	if err == nil {
		// Gateway and HTTPRoute features require their complete core conformance
		// profiles. The current HTTP subset does not justify declaring either.
		controller.SetCondition(&class.Status.Conditions, controller.Condition(
			string(gatewayv1.GatewayClassConditionStatusAccepted), metav1.ConditionTrue,
			string(gatewayv1.GatewayClassReasonAccepted), "GatewayClass parameters and Amphora topology are accepted", class.Generation,
		))
		return ctrl.Result{RequeueAfter: controller.OpenStackResyncAfter(controller.DefaultOpenStackResyncInterval, class.UID)}, nil
	}
	var invalid *classconfig.InvalidParametersError
	if errors.As(err, &invalid) {
		controller.SetCondition(&class.Status.Conditions, controller.Condition(
			string(gatewayv1.GatewayClassConditionStatusAccepted), metav1.ConditionFalse,
			string(gatewayv1.GatewayClassReasonInvalidParameters), invalid.Message, class.Generation,
		))
		return ctrl.Result{}, nil
	}
	policy, classified := controller.ProviderFailurePolicyFor(err)
	if !classified {
		return ctrl.Result{}, err
	}
	conditionType := string(gatewayv1.GatewayClassConditionStatusAccepted)
	observedGeneration := controller.ConditionObservedGeneration(class.Status.Conditions, conditionType, class.Generation, policy.AdvancesObservedGeneration)
	emitEvent := controller.ConditionTransitioned(class.Status.Conditions, conditionType, policy.ConditionStatus, policy.Reason, policy.Message, observedGeneration)

	controller.SetCondition(&class.Status.Conditions, controller.Condition(
		string(gatewayv1.GatewayClassConditionStatusAccepted), policy.ConditionStatus,
		policy.Reason, policy.Message, observedGeneration,
	))
	if emitEvent && policy.EmitEvent {
		scope.warning = &policy
	}
	return controller.ProviderFailureResult(policy, err, string(class.UID))
}

func gatewayClassRequestsForGateway(_ context.Context, object client.Object) []reconcile.Request {
	gateway, ok := object.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)},
	}}
}

func (r *Reconciler) gatewayClassRequestsForGatewayAPICRD(ctx context.Context, object client.Object) []reconcile.Request {
	if object == nil || !controller.IsGatewayAPICRDName(object.GetName()) {
		return nil
	}
	var gatewayClasses gatewayv1.GatewayClassList
	if err := r.List(ctx, &gatewayClasses, client.MatchingFields{
		controller.IndexGatewayClassByController: string(r.Config.ControllerName),
	}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list GatewayClasses for Gateway API CRD")
		return nil
	}
	keys := make(map[types.NamespacedName]struct{}, len(gatewayClasses.Items))
	for _, gatewayClass := range gatewayClasses.Items {
		keys[types.NamespacedName{Name: gatewayClass.Name}] = struct{}{}
	}
	return controller.SortedRequests(keys)
}

// SetupWithManager registers GatewayClass reconciliation and requeues a class
// whenever one of its referencing Gateways changes.
func (r *Reconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.APIReader == nil {
		return controller.ErrAPIReaderRequired
	}

	definition := &metav1.PartialObjectMetadata{}
	definition.SetGroupVersionKind(
		apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"),
	)
	return ctrl.NewControllerManagedBy(manager).
		For(&gatewayv1.GatewayClass{}, builder.WithPredicates(controller.GatewayClassReconcilePredicate())).
		Watches(&configv1alpha1.OpenStackGatewayClassConfig{}, handler.EnqueueRequestsFromMapFunc(r.gatewayClassRequestsForParameters), builder.WithPredicates(controller.GenerationOrDeletionChangedPredicate())).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(gatewayClassRequestsForGateway),
			builder.WithPredicates(controller.GenerationOrDeletionChangedPredicate()),
		).
		WatchesMetadata(
			definition,
			handler.EnqueueRequestsFromMapFunc(r.gatewayClassRequestsForGatewayAPICRD),
			builder.WithPredicates(controller.GatewayAPICRDReconcilePredicate()),
		).
		Named("gatewayclass").
		Complete(r)
}
