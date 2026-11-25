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
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestHTTPRouteDeletionProgressRetainsFinalizersAndBinding(t *testing.T) {
	cfg := testConfig()
	route, annotations := deletingBoundRoute(cfg)
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(route).
		Build()
	deleteOutcome := cloud.ProgressingOutcome("Deleting controller-owned L7 rule", time.Hour)
	provider := &recordingProvider{}
	provider.Respond = singleRouteGraphResponse(t, cloud.RouteResult{Outcome: deleteOutcome}, nil)
	reconciler := *newTestReconciler(Reconciler{
		Client:    kubeClient,
		APIReader: kubeClient,

		Coordinator: &graph.Coordinator{},
		Config:      cfg,
	}, provider)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantRequeue := controller.ProviderProgressRequeueAfter(deleteOutcome, route.UID)
	if result.RequeueAfter != wantRequeue {
		t.Fatalf("Reconcile() RequeueAfter = %s, want %s", result.RequeueAfter, wantRequeue)
	}
	if len(provider.DeletedRoutes()) != 1 {
		t.Fatalf("DeleteRoute calls = %d, want 1", len(provider.DeletedRoutes()))
	}

	current := &gatewayv1.HTTPRoute{}
	if err := kubeClient.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatalf("get HTTPRoute after progressing deletion: %v", err)
	}
	for _, finalizer := range route.Finalizers {
		if !controllerutil.ContainsFinalizer(current, finalizer) {
			t.Errorf("progressing deletion removed finalizer %q", finalizer)
		}
	}
	for annotation, want := range annotations {
		if got := current.Annotations[annotation]; got != want {
			t.Errorf("HTTPRoute annotation %q = %q, want %q", annotation, got, want)
		}
	}
}

func TestHTTPRouteDeletionRemovesBindingOnlyAfterProviderIsReady(t *testing.T) {
	cfg := testConfig()
	route, annotations := deletingBoundRoute(cfg)
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).
		WithStatusSubresource(route).
		WithObjects(route).
		Build()
	deleteOutcomes := []cloud.Outcome{
		cloud.ProgressingOutcome("Deleting controller-owned L7 rule", time.Second),
		cloud.ReadyOutcome(),
	}
	provider := &recordingProvider{}
	provider.Respond = func(ctx context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
		index := min(len(provider.Calls)-1, len(deleteOutcomes)-1)
		return singleRouteGraphResponse(t, cloud.RouteResult{Outcome: deleteOutcomes[index]}, nil)(ctx, spec)
	}
	reconciler := *newTestReconciler(Reconciler{
		Client:    kubeClient,
		APIReader: kubeClient,

		Coordinator: &graph.Coordinator{},
		Config:      cfg,
	}, provider)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: route.Namespace, Name: route.Name}}

	first, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if first.RequeueAfter == 0 {
		t.Fatalf("first Reconcile() result = %#v, want a delayed observation", first)
	}
	second, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if second != (ctrl.Result{}) {
		t.Fatalf("second Reconcile() result = %#v, want no requeue", second)
	}
	if len(provider.DeletedRoutes()) != 2 {
		t.Fatalf("DeleteRoute calls = %d, want 2", len(provider.DeletedRoutes()))
	}

	current := &gatewayv1.HTTPRoute{}
	err = kubeClient.Get(context.Background(), request.NamespacedName, current)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Fatalf("get HTTPRoute after completed deletion: %v", err)
	}
	for _, finalizer := range route.Finalizers {
		if controllerutil.ContainsFinalizer(current, finalizer) {
			t.Errorf("completed deletion retained finalizer %q", finalizer)
		}
	}
	for annotation := range annotations {
		if _, exists := current.Annotations[annotation]; exists {
			t.Errorf("completed deletion retained annotation %q", annotation)
		}
	}
}

func deletingBoundRoute(cfg Config) (*gatewayv1.HTTPRoute, map[string]string) {
	now := metav1.Now()
	annotations := map[string]string{
		cfg.RouteBindingsAnnotation(): routeBindingJSON(cfg, cloud.Identity{OpenStackProjectID: cfg.OpenStackProjectID, ClusterID: cfg.ClusterID, GatewayNamespace: "default", GatewayName: "edge", GatewayUID: "gateway-uid", RouteNamespace: "default", RouteName: "api", RouteUID: "route-uid"}, controller.RouteDetaching, ""),
	}
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "api",
			UID:               "route-uid",
			Generation:        1,
			DeletionTimestamp: &now,
			Finalizers:        routeBindingFinalizers(cfg, "default", "edge", "gateway-uid"),
			Annotations:       annotations,
		},
	}, annotations
}
