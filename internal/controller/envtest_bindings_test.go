//go:build envtest

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

package controller_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// testHTTPRouteBindingConflictsAndIndexes exercises API-server resource versions
// and cache index updates that a fake Kubernetes client cannot establish.
func testHTTPRouteBindingConflictsAndIndexes(t *testing.T, apiClient, cacheClient client.Client, config controller.Config) {
	t.Helper()
	for _, name := range []string{"binding-routes-envtest", "binding-gateways-envtest"} {
		if err := apiClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatalf("create binding test Namespace: %v", err)
		}
	}
	firstGateway := createEnvtestGateway(t, apiClient, cacheClient, "binding-routes-envtest", "first", "binding-class-envtest")
	secondGateway := createEnvtestGateway(t, apiClient, cacheClient, "binding-gateways-envtest", "second", "binding-class-envtest")
	route := envtestHTTPRoute(firstGateway.Namespace, "bindings", firstGateway.Name, "backend")
	route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{
		Group: ptr.To(gatewayv1.Group(gatewayv1.GroupName)), Kind: ptr.To(gatewayv1.Kind("Gateway")),
		Namespace: ptr.To(gatewayv1.Namespace(secondGateway.Namespace)), Name: gatewayv1.ObjectName(secondGateway.Name),
	})
	const foreignFinalizer = "other.example.test/retain"
	route.Finalizers = []string{foreignFinalizer}
	route.Annotations = map[string]string{"other.example.test/note": "preserve"}
	if err := apiClient.Create(t.Context(), route); err != nil {
		t.Fatalf("create binding test HTTPRoute: %v", err)
	}
	route.Status.Parents = []gatewayv1.RouteParentStatus{
		{ParentRef: route.Spec.ParentRefs[0], ControllerName: "other.example.test/controller"},
		{ParentRef: route.Spec.ParentRefs[1], ControllerName: config.ControllerName},
	}
	for i := range route.Status.Parents {
		route.Status.Parents[i].Conditions = []metav1.Condition{{
			Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue,
			Reason: string(gatewayv1.RouteReasonAccepted), Message: "Preserve this parent status",
			ObservedGeneration: route.Generation, LastTransitionTime: metav1.Now(),
		}}
	}
	if err := apiClient.Status().Update(t.Context(), route); err != nil {
		t.Fatalf("seed parent status: %v", err)
	}
	key := client.ObjectKeyFromObject(route)
	getRoute := func() *gatewayv1.HTTPRoute {
		t.Helper()
		current := &gatewayv1.HTTPRoute{}
		if err := apiClient.Get(t.Context(), key, current); err != nil {
			t.Fatalf("get binding test HTTPRoute: %v", err)
		}
		return current
	}
	patch := func(base, desired *gatewayv1.HTTPRoute) {
		t.Helper()
		if err := apiClient.Patch(t.Context(), desired, controller.OptimisticMergeFrom(base)); err != nil {
			t.Fatalf("patch HTTPRoute binding: %v", err)
		}
	}

	base := getRoute()
	originalStatus := base.Status.DeepCopy()
	firstBinding := controller.RouteBinding{Identity: controller.RouteIdentity(config, firstGateway, base), Lifecycle: controller.RouteAttached}
	secondBinding := controller.RouteBinding{Identity: controller.RouteIdentity(config, secondGateway, base), Lifecycle: controller.RouteAttached}
	first, second := base.DeepCopy(), base.DeepCopy()
	if err := controller.SetRouteBinding(config, first, firstBinding); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetRouteBinding(config, second, secondBinding); err != nil {
		t.Fatal(err)
	}
	patch(base, first)
	// Both writers prepared from the same revision. The second must not replace
	// the first writer's whole binding list or its finalizer set.
	if err := apiClient.Patch(t.Context(), second, controller.OptimisticMergeFrom(base)); !apierrors.IsConflict(err) {
		t.Fatalf("second stale binding patch = %v, want resourceVersion conflict", err)
	}
	current := getRoute()
	bindings, err := controller.StoredRouteBindings(config, current)
	if err != nil || len(bindings) != 1 || bindings[0].Identity.GatewayUID != firstBinding.Identity.GatewayUID {
		t.Fatalf("conflicting patch changed the persisted binding: %#v, %v", bindings, err)
	}
	base, second = current.DeepCopy(), current.DeepCopy()
	if err := controller.SetRouteBinding(config, second, secondBinding); err != nil {
		t.Fatal(err)
	}
	patch(base, second)
	current = getRoute()
	bindings, err = controller.StoredRouteBindings(config, current)
	if err != nil || len(bindings) != 2 {
		t.Fatalf("refetched binding patch lost an entry: %#v, %v", bindings, err)
	}
	if !controllerutil.ContainsFinalizer(current, foreignFinalizer) || current.Annotations["other.example.test/note"] != "preserve" || !equality.Semantic.DeepEqual(current.Status, *originalStatus) {
		t.Fatalf("binding additions changed foreign metadata or parent status: %#v", current)
	}

	assertIndex := func(field, value string, present bool) {
		t.Helper()
		waitForEnvtest(t, func(ctx context.Context) (bool, error) {
			var routes gatewayv1.HTTPRouteList
			if err := cacheClient.List(ctx, &routes, client.MatchingFields{field: value}); err != nil {
				return false, err
			}
			if !present {
				return len(routes.Items) == 0, nil
			}
			return len(routes.Items) == 1 && routes.Items[0].UID == route.UID, nil
		})
	}
	for _, gateway := range []*gatewayv1.Gateway{firstGateway, secondGateway} {
		gatewayKey := controller.ObjectKeyString(client.ObjectKeyFromObject(gateway))
		assertIndex(controller.IndexHTTPRouteByParentGateway, gatewayKey, true)
		assertIndex(controller.IndexHTTPRouteByBoundGateway, gatewayKey, true)
		assertIndex(controller.IndexHTTPRouteByBoundGatewayUID, string(gateway.UID), true)
	}

	// Removing a ParentRef must not erase the durable cleanup entry or its
	// indexes before that Gateway's cloud fragment has been confirmed absent.
	base = current.DeepCopy()
	current.Spec.ParentRefs = current.Spec.ParentRefs[1:]
	patch(base, current)
	firstKey := controller.ObjectKeyString(client.ObjectKeyFromObject(firstGateway))
	secondKey := controller.ObjectKeyString(client.ObjectKeyFromObject(secondGateway))
	assertIndex(controller.IndexHTTPRouteByParentGateway, firstKey, false)
	assertIndex(controller.IndexHTTPRouteByBoundGateway, firstKey, true)
	assertIndex(controller.IndexHTTPRouteByBoundGatewayUID, string(firstGateway.UID), true)
	assertIndex(controller.IndexHTTPRouteByParentGateway, secondKey, true)

	current = getRoute()
	base = current.DeepCopy()
	firstBinding.Lifecycle = controller.RouteDetaching
	if err := controller.SetRouteBinding(config, current, firstBinding); err != nil {
		t.Fatal(err)
	}
	patch(base, current)
	current = getRoute()
	base = current.DeepCopy()
	// No cloud resources are created in this metadata-only scenario.
	if err := controller.RemoveRouteBinding(config, current, string(firstGateway.UID)); err != nil {
		t.Fatal(err)
	}
	patch(base, current)
	current = getRoute()
	bindings, err = controller.StoredRouteBindings(config, current)
	if err != nil || len(bindings) != 1 || bindings[0] != secondBinding || !controllerutil.ContainsFinalizer(current, config.RouteFinalizer()) {
		t.Fatalf("cleaning one Gateway changed the surviving binding: %#v, %v", bindings, err)
	}
	if !controllerutil.ContainsFinalizer(current, foreignFinalizer) || !equality.Semantic.DeepEqual(current.Status, *originalStatus) {
		t.Fatalf("binding cleanup changed another owner's finalizer or parent status: %#v", current)
	}
	assertIndex(controller.IndexHTTPRouteByBoundGateway, firstKey, false)
	assertIndex(controller.IndexHTTPRouteByBoundGatewayUID, string(firstGateway.UID), false)
	assertIndex(controller.IndexHTTPRouteByBoundGateway, secondKey, true)
	assertIndex(controller.IndexHTTPRouteByBoundGatewayUID, string(secondGateway.UID), true)
	assertIndex(controller.IndexHTTPRouteByParentGateway, secondKey, true)

	base = current.DeepCopy()
	if err := controller.RemoveRouteBinding(config, current, string(secondGateway.UID)); err != nil {
		t.Fatal(err)
	}
	patch(base, current)
	current = getRoute()
	if controller.RouteHasControllerBinding(config, current) || !controllerutil.ContainsFinalizer(current, foreignFinalizer) {
		t.Fatalf("last binding cleanup left controller metadata or removed a foreign finalizer: %#v", current)
	}
	assertIndex(controller.IndexHTTPRouteByBoundGateway, secondKey, false)
	assertIndex(controller.IndexHTTPRouteByBoundGatewayUID, string(secondGateway.UID), false)
	assertIndex(controller.IndexHTTPRouteByParentGateway, secondKey, true)
}
