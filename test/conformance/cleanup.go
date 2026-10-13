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

package diagnostic

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type createdObjects struct {
	uncertainCreate bool
	runID           string
	namespace       *corev1.Namespace
	gateway         *gatewayv1.Gateway
	route           *gatewayv1.HTTPRoute
}

func (created *createdObjects) record(object client.Object) error {
	if object.GetUID() == "" {
		return fmt.Errorf("created object has no server-assigned UID")
	}
	switch value := object.(type) {
	case *corev1.Namespace:
		created.namespace = value.DeepCopy()
	case *gatewayv1.Gateway:
		created.gateway = value.DeepCopy()
	case *gatewayv1.HTTPRoute:
		created.route = value.DeepCopy()
	}
	return nil
}

// cleanup waits for controller finalization before removing the namespace.
// A failed or ambiguous create is never adopted into the UID ledger.
func (created *createdObjects) cleanup(ctx context.Context, c client.Client, poll time.Duration) error {
	if created.uncertainCreate {
		return fmt.Errorf("fixture creation was not confirmed; retain objects for inspection")
	}
	if created.namespace == nil {
		return nil
	}
	if err := requireOwnedObject(ctx, c, created.namespace, created.runID); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := created.requireKnownGraphs(ctx, c); err != nil {
		return err
	}
	if created.route != nil {
		if err := deleteAndWait(ctx, c, created.route, created.runID, poll); err != nil {
			return err
		}
	}
	if created.gateway != nil {
		if err := deleteAndWait(ctx, c, created.gateway, created.runID, poll); err != nil {
			return err
		}
	}
	// Re-observe after asynchronous cleanup. Retain the namespace if another
	// graph appeared, including one from an uncertain create response.
	if err := created.requireNoGraphs(ctx, c); err != nil {
		return err
	}
	var services corev1.ServiceList
	if err := c.List(ctx, &services, client.InNamespace(created.namespace.Name)); err != nil {
		return fmt.Errorf("observe Services before namespace deletion: %w", err)
	}
	for _, service := range services.Items {
		if service.Spec.Type == corev1.ServiceTypeLoadBalancer {
			return fmt.Errorf("namespace contains a LoadBalancer Service; retain it to protect OCCM resources")
		}
	}
	return deleteAndWait(ctx, c, created.namespace, created.runID, poll)
}

func (created *createdObjects) requireKnownGraphs(ctx context.Context, c client.Reader) error {
	var gateways gatewayv1.GatewayList
	if err := c.List(ctx, &gateways, client.InNamespace(created.namespace.Name)); err != nil {
		return fmt.Errorf("observe Gateways before cleanup: %w", err)
	}
	for i := range gateways.Items {
		if created.gateway == nil || gateways.Items[i].UID != created.gateway.UID {
			return fmt.Errorf("namespace contains a Gateway absent from the creation ledger")
		}
	}
	var routes gatewayv1.HTTPRouteList
	if err := c.List(ctx, &routes, client.InNamespace(created.namespace.Name)); err != nil {
		return fmt.Errorf("observe HTTPRoutes before cleanup: %w", err)
	}
	for i := range routes.Items {
		if created.route == nil || routes.Items[i].UID != created.route.UID {
			return fmt.Errorf("namespace contains an HTTPRoute absent from the creation ledger")
		}
	}
	return nil
}

func (created *createdObjects) requireNoGraphs(ctx context.Context, c client.Reader) error {
	var gateways gatewayv1.GatewayList
	if err := c.List(ctx, &gateways, client.InNamespace(created.namespace.Name)); err != nil {
		return fmt.Errorf("confirm Gateway absence: %w", err)
	}
	var routes gatewayv1.HTTPRouteList
	if err := c.List(ctx, &routes, client.InNamespace(created.namespace.Name)); err != nil {
		return fmt.Errorf("confirm HTTPRoute absence: %w", err)
	}
	if len(gateways.Items) != 0 || len(routes.Items) != 0 {
		return fmt.Errorf("owned namespace still contains a graph; retain it for controller cleanup")
	}
	return nil
}

func requireOwnedObject(ctx context.Context, reader client.Reader, recorded client.Object, runID string) error {
	fresh := recorded.DeepCopyObject().(client.Object)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(recorded), fresh); err != nil {
		return err
	}
	if recorded.GetUID() == "" || fresh.GetUID() != recorded.GetUID() || fresh.GetAnnotations()[runAnnotation] != runID {
		return fmt.Errorf("cleanup identity changed; retain the object")
	}
	return nil
}

func deleteAndWait(ctx context.Context, c client.Client, recorded client.Object, runID string, poll time.Duration) error {
	if err := requireOwnedObject(ctx, c, recorded, runID); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	uid := recorded.GetUID()
	if err := c.Delete(ctx, recorded, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("request owned object deletion: %w", err)
	}
	return wait.PollUntilContextCancel(ctx, poll, true, func(ctx context.Context) (bool, error) {
		err := requireOwnedObject(ctx, c, recorded, runID)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}
