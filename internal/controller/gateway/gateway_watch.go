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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

func (r *Reconciler) gatewayRequestsForHTTPRoute(_ context.Context, object client.Object) []reconcile.Request {
	route, ok := object.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}

	keys := make(map[types.NamespacedName]struct{}, len(route.Spec.ParentRefs)+len(route.Status.Parents)+1)
	addParent := func(parentRef gatewayv1.ParentReference) {
		if !controller.IsGatewayParentRef(parentRef) || parentRef.Name == "" {
			return
		}
		gatewayNamespace := route.Namespace
		if parentRef.Namespace != nil {
			gatewayNamespace = string(*parentRef.Namespace)
		}
		keys[types.NamespacedName{Namespace: gatewayNamespace, Name: string(parentRef.Name)}] = struct{}{}
	}
	for _, parentRef := range route.Spec.ParentRefs {
		addParent(parentRef)
	}
	for _, parentStatus := range route.Status.Parents {
		if parentStatus.ControllerName != r.Config.ControllerName {
			continue
		}
		addParent(parentStatus.ParentRef)
	}
	bindings, err := controller.StoredRouteBindings(r.Config, route)
	if err == nil {
		for _, binding := range bindings {
			keys[types.NamespacedName{Namespace: binding.Identity.GatewayNamespace, Name: binding.Identity.GatewayName}] = struct{}{}
		}
	}
	return controller.SortedRequests(keys)
}

func (r *Reconciler) gatewayRequestsForGatewayClass(ctx context.Context, object client.Object) []reconcile.Request {
	gatewayClass, ok := object.(*gatewayv1.GatewayClass)
	if !ok {
		return nil
	}

	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.MatchingFields{
		controller.IndexGatewayByClass: gatewayClass.Name,
	}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list Gateways for GatewayClass", "gatewayClass", gatewayClass.Name)
		return nil
	}
	keys := make(map[types.NamespacedName]struct{}, len(gateways.Items))
	for _, gateway := range gateways.Items {
		keys[client.ObjectKeyFromObject(&gateway)] = struct{}{}
	}
	return controller.SortedRequests(keys)
}

// SetupWithManager registers Gateway reconciliation and dependency watches.
func (r *Reconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Coordinator == nil {
		return graph.ErrCoordinatorRequired
	}
	if r.APIReader == nil {
		return controller.ErrAPIReaderRequired
	}
	if r.Writer == nil {
		return errors.New("gateway graph writer is required")
	}
	return ctrl.NewControllerManagedBy(manager).
		For(&gatewayv1.Gateway{}, builder.WithPredicates(controller.GatewayReconcilePredicate(r.Config))).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayRequestsForHTTPRoute),
			builder.WithPredicates(controller.HTTPRouteForGatewayPredicate(r.Config)),
		).
		Watches(
			&gatewayv1.GatewayClass{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayRequestsForGatewayClass),
			builder.WithPredicates(controller.GatewayClassForGatewayPredicate()),
		).
		Watches(&configv1alpha1.OpenStackGatewayClassConfig{}, handler.EnqueueRequestsFromMapFunc(r.gatewayRequestsForParameters), builder.WithPredicates(controller.GenerationOrDeletionChangedPredicate())).
		// Secret data is read through APIReader only after reference authorization.
		WatchesMetadata(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.gatewayRequestsForCertificateSecret), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&gatewayv1.ReferenceGrant{}, handler.EnqueueRequestsFromMapFunc(r.gatewayRequestsForCertificateGrant), builder.WithPredicates(controller.GenerationOrDeletionChangedPredicate())).
		Named("gateway").
		Complete(r)
}

func (r *Reconciler) gatewayRequestsForCertificateSecret(ctx context.Context, object client.Object) []reconcile.Request {
	if object == nil {
		return nil
	}
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.MatchingFields{
		controller.IndexGatewayByCertificateSecret: controller.ObjectKeyString(client.ObjectKeyFromObject(object)),
	}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list Gateways for certificate Secret")
		return nil
	}
	return r.managedGatewayRequests(ctx, gateways.Items)
}

func (r *Reconciler) gatewayRequestsForCertificateGrant(ctx context.Context, object client.Object) []reconcile.Request {
	grant, ok := object.(*gatewayv1.ReferenceGrant)
	if !ok {
		return nil
	}
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.MatchingFields{
		controller.IndexGatewayByCertificateNamespace: grant.Namespace,
	}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list Gateways for certificate ReferenceGrant")
		return nil
	}
	var affected []gatewayv1.Gateway
	for _, gateway := range gateways.Items {
		if gatewayCertificateGrantApplies(&gateway, *grant) {
			affected = append(affected, gateway)
		}
	}
	return r.managedGatewayRequests(ctx, affected)
}

func gatewayCertificateGrantApplies(gateway *gatewayv1.Gateway, grant gatewayv1.ReferenceGrant) bool {
	// A deleting grant still identifies the references that must be reevaluated.
	// This event mapping does not authorize reading certificate payloads.
	grant.DeletionTimestamp = nil
	for _, listener := range gateway.Spec.Listeners {
		if listener.Protocol != gatewayv1.HTTPSProtocolType || listener.TLS == nil {
			continue
		}
		for _, reference := range listener.TLS.CertificateRefs {
			key, supported := model.CertificateSecretReference(gateway.Namespace, reference)
			if supported && key.Namespace != gateway.Namespace && key.Namespace == grant.Namespace &&
				model.CertificateReferenceAllowed(gateway.Namespace, reference, []gatewayv1.ReferenceGrant{grant}) {
				return true
			}
		}
	}
	return false
}

func (r *Reconciler) managedGatewayRequests(ctx context.Context, gateways []gatewayv1.Gateway) []reconcile.Request {
	classes := make(map[gatewayv1.ObjectName]bool)
	keys := make(map[types.NamespacedName]struct{})
	for _, gateway := range gateways {
		if !gateway.DeletionTimestamp.IsZero() {
			continue
		}
		managed, known := classes[gateway.Spec.GatewayClassName]
		if !known {
			_, selected, err := r.getGatewayClass(ctx, &gateway)
			if err != nil {
				log.FromContext(ctx).Error(err, "Could not read GatewayClass for certificate event", "gatewayClass", gateway.Spec.GatewayClassName)
			}
			managed = err == nil && selected
			classes[gateway.Spec.GatewayClassName] = managed
		}
		if managed {
			keys[client.ObjectKeyFromObject(&gateway)] = struct{}{}
		}
	}
	return controller.SortedRequests(keys)
}

// Parameter updates can leave GatewayClass conditions unchanged. Wake unbound
// Gateways directly so they cannot remain rejected after the inputs recover.
func (r *Reconciler) gatewayRequestsForParameters(ctx context.Context, object client.Object) []reconcile.Request {
	if object == nil {
		return nil
	}
	var classes gatewayv1.GatewayClassList
	if err := r.List(ctx, &classes, client.MatchingFields{controller.IndexGatewayClassByParameters: object.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list GatewayClasses for parameters")
		return nil
	}
	keys := make(map[types.NamespacedName]struct{})
	for _, class := range classes.Items {
		if class.Spec.ControllerName != r.Config.ControllerName {
			continue
		}
		var gateways gatewayv1.GatewayList
		if err := r.List(ctx, &gateways, client.MatchingFields{controller.IndexGatewayByClass: class.Name}); err != nil {
			log.FromContext(ctx).Error(err, "Could not list Gateways for class parameters", "gatewayClass", class.Name)
			continue
		}
		for _, gateway := range gateways.Items {
			if !controller.HasGatewayTopologyBinding(r.Config, &gateway) && gateway.DeletionTimestamp.IsZero() {
				keys[client.ObjectKeyFromObject(&gateway)] = struct{}{}
			}
		}
	}
	return controller.SortedRequests(keys)
}
