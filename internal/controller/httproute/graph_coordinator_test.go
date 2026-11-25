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
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/internal/controllertest"
)

func TestReconcilersShareGatewayGraphCoordinator(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	config := testConfig()
	coordinator := &graph.Coordinator{}
	provider := newBlockingGraphProvider()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "edge",
			UID:         "gateway-1",
			Generation:  1,
			Finalizers:  []string{config.GatewayFinalizer()},
			Annotations: gatewayBindingAnnotations(config, "80"),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "openstack",
			Listeners:        []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
		},
	}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-1"
	route.Generation = 1
	routeBinder := newTestReconciler(Reconciler{Config: config}, nil)
	if err := routeBinder.applyRouteBinding(route, gateway); err != nil {
		t.Fatal(err)
	}
	updateBindingFixture(t, config, route.Annotations, func(binding *controller.RouteBinding) { binding.Lifecycle = controller.RouteDetaching })
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(route).Build()
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
		t.Fatal(err)
	}
	routeReconciler := newTestReconciler(Reconciler{Client: kubeClient, Coordinator: coordinator, APIReader: kubeClient, Config: config}, provider)
	storedRouteIdentity, present, err := routeReconciler.storedRouteIdentity(route, string(gateway.UID))
	if err != nil || !present {
		t.Fatalf("stored route identity = %#v, present %t, error %v", storedRouteIdentity, present, err)
	}
	releaseGatewayWriter, err := graph.Acquire(ctx, coordinator, string(gateway.UID))
	if err != nil {
		t.Fatalf("acquire Gateway graph: %v", err)
	}
	t.Cleanup(releaseGatewayWriter)

	routeDone := make(chan error, 1)
	go func() {
		_, err := routeReconciler.deleteRoute(ctx, route.DeepCopy(), storedRouteIdentity)
		routeDone <- err
	}()
	select {
	case <-provider.routeObserved:
		t.Fatal("HTTPRoute observed the graph while Gateway mutation was active")
	case <-ctx.Done():
		t.Fatalf("wait for blocked HTTPRoute reconciliation: %v", ctx.Err())
	case <-time.After(25 * time.Millisecond):
	}

	releaseGatewayWriter()
	select {
	case <-provider.routeObserved:
	case <-ctx.Done():
		t.Fatalf("HTTPRoute did not observe the graph after Gateway mutation completed: %v", ctx.Err())
	}
	select {
	case err := <-routeDone:
		if err != nil {
			t.Fatalf("delete HTTPRoute graph: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("HTTPRoute reconciliation did not finish: %v", ctx.Err())
	}
}

func TestHTTPRouteRevalidatesSnapshotAfterWaitingForGraph(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	config := testConfig()
	coordinator := &graph.Coordinator{}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1", Generation: 1}}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-1"
	route.Generation = 1
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(gateway, route).Build()
	provider := &recordingProvider{}
	reconciler := newTestReconciler(Reconciler{Client: kubeClient, Coordinator: coordinator, APIReader: kubeClient, Config: config}, provider)

	release, err := graph.Acquire(ctx, coordinator, string(gateway.UID))
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}
	t.Cleanup(release)
	result := make(chan error, 1)
	go func() {
		_, err := reconciler.ensureRouteGraph(ctx, route.DeepCopy(), gateway.DeepCopy())
		result <- err
	}()
	var changed gatewayv1.HTTPRoute
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(route), &changed); err != nil {
		t.Fatal(err)
	}
	changed.Generation = 2
	changed.Spec.Hostnames = []gatewayv1.Hostname{"new.example.test"}
	if err := kubeClient.Update(ctx, &changed); err != nil {
		t.Fatal(err)
	}
	release()

	select {
	case err := <-result:
		if !errors.Is(err, errHTTPRouteChanged) {
			t.Fatalf("ensureRouteGraph() error = %v, want %v", err, errHTTPRouteChanged)
		}
	case <-ctx.Done():
		t.Fatalf("HTTPRoute reconciliation did not finish: %v", ctx.Err())
	}
	if len(provider.RouteSpecs()) != 0 {
		t.Fatalf("EnsureRoute calls = %d, want 0", len(provider.RouteSpecs()))
	}
}

func TestHTTPRouteDeleteRevalidatesBindingAfterWaitingForGraph(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	config := testConfig()
	coordinator := &graph.Coordinator{}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-1"}}
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-1"
	route.Generation = 1
	binder := newTestReconciler(Reconciler{Config: config}, nil)
	if err := binder.applyRouteBinding(route, gateway); err != nil {
		t.Fatal(err)
	}
	kubeClient := indexedFakeClientBuilder(testScheme(t), config).WithObjects(route).Build()
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
		t.Fatal(err)
	}
	provider := &recordingProvider{}
	reconciler := newTestReconciler(Reconciler{Client: kubeClient, Coordinator: coordinator, APIReader: kubeClient, Config: config}, provider)
	stored, present, err := reconciler.storedRouteIdentity(route, string(gateway.UID))
	if err != nil || !present {
		t.Fatalf("stored route identity = %#v, present %t, error %v", stored, present, err)
	}

	release, err := graph.Acquire(ctx, coordinator, string(gateway.UID))
	if err != nil {
		t.Fatalf("acquire graph lock: %v", err)
	}
	t.Cleanup(release)
	result := make(chan error, 1)
	go func() {
		_, err := reconciler.deleteRoute(ctx, route.DeepCopy(), stored)
		result <- err
	}()
	var changed gatewayv1.HTTPRoute
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(route), &changed); err != nil {
		t.Fatal(err)
	}
	changed.Generation = 2
	changed.Spec.Hostnames = []gatewayv1.Hostname{"new.example.test"}
	if err := kubeClient.Update(ctx, &changed); err != nil {
		t.Fatal(err)
	}
	release()

	select {
	case err := <-result:
		if !errors.Is(err, errHTTPRouteChanged) {
			t.Fatalf("deleteRoute() error = %v, want %v", err, errHTTPRouteChanged)
		}
	case <-ctx.Done():
		t.Fatalf("HTTPRoute cleanup did not finish: %v", ctx.Err())
	}
	if len(provider.DeletedRoutes()) != 0 {
		t.Fatalf("DeleteRoute calls = %d, want 0", len(provider.DeletedRoutes()))
	}
}

type blockingGraphProvider struct {
	routeObserved chan struct{}
	routeOnce     sync.Once
}

func newBlockingGraphProvider() *blockingGraphProvider {
	return &blockingGraphProvider{
		routeObserved: make(chan struct{}),
	}
}

func (p *blockingGraphProvider) ReconcileGatewayGraph(_ context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	for _, route := range spec.Routes {
		if route.Disposition == cloud.GraphDelete {
			p.routeOnce.Do(func() { close(p.routeObserved) })
		}
	}
	return controllertest.ReadyGraphResult(spec), nil
}
