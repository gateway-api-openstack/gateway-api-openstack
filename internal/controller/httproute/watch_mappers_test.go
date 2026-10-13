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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

func TestHTTPRouteRequestsForGatewayUseParentAndStoredBinding(t *testing.T) {
	config := testConfig()
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gateways", Name: "edge"}}
	parentNamespace := gatewayv1.Namespace(gateway.Namespace)
	direct := mapperTestHTTPRoute("routes", "direct", gatewayv1.ParentReference{
		Name:      gatewayv1.ObjectName(gateway.Name),
		Namespace: &parentNamespace,
	}, "backend")
	bound := mapperTestHTTPRoute("routes", "bound", gatewayv1.ParentReference{Name: "other"}, "backend")
	bound.Annotations = map[string]string{
		config.RouteGatewayNamespaceAnnotation(): gateway.Namespace,
		config.RouteGatewayNameAnnotation():      gateway.Name,
		config.RouteGatewayUIDAnnotation():       "gateway-uid",
	}
	status := mapperTestHTTPRoute("routes", "status", gatewayv1.ParentReference{Name: "other"}, "backend")
	status.Status.Parents = []gatewayv1.RouteParentStatus{{
		ParentRef: gatewayv1.ParentReference{
			Name:      gatewayv1.ObjectName(gateway.Name),
			Namespace: &parentNamespace,
		},
		ControllerName: config.ControllerName,
	}}
	unrelated := mapperTestHTTPRoute("routes", "unrelated", gatewayv1.ParentReference{Name: "other"}, "backend")

	kubeClient := indexedFakeClientBuilder(testScheme(t), config).
		WithObjects(gateway, direct, bound, status, unrelated).
		Build()
	reconciler := newTestReconciler(Reconciler{Client: kubeClient, Config: config}, nil)

	got := reconciler.enqueueHTTPRoutesForGateway(context.Background(), gateway)
	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "routes", Name: "bound"}},
		{NamespacedName: types.NamespacedName{Namespace: "routes", Name: "direct"}},
		{NamespacedName: types.NamespacedName{Namespace: "routes", Name: "status"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueueHTTPRoutesForGateway() = %#v, want %#v", got, want)
	}
}

func TestHTTPRouteRequestsForServiceKeepDirectRouteWhenPeerLookupFails(t *testing.T) {
	config := testConfig()
	direct := mapperTestHTTPRoute("default", "direct", gatewayv1.ParentReference{Name: "edge"}, "changed")
	second := mapperTestHTTPRoute("default", "second", gatewayv1.ParentReference{Name: "edge"}, "changed")
	peer := mapperTestHTTPRoute("default", "peer", gatewayv1.ParentReference{Name: "edge"}, "stable")
	baseClient := indexedFakeClientBuilder(testScheme(t), config).
		WithObjects(direct, second, peer).
		Build()
	kubeClient := &failNthListClient{Client: baseClient, failAt: 2, err: errors.New("cache unavailable")}
	reconciler := newTestReconciler(Reconciler{Client: kubeClient, Config: config}, nil)

	got := reconciler.enqueueHTTPRoutesForServiceKey(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      "changed",
	})
	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "direct"}},
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "second"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueueHTTPRoutesForServiceKey() after peer lookup failure = %#v, want %#v", got, want)
	}
	if kubeClient.calls != 2 {
		t.Fatalf("list calls = %d, want one backend and one parent lookup", kubeClient.calls)
	}
	got = reconciler.enqueueHTTPRoutesForServiceKey(context.Background(), types.NamespacedName{Namespace: "default", Name: "changed"})
	want = []reconcile.Request{
		{NamespacedName: client.ObjectKeyFromObject(direct)},
		{NamespacedName: client.ObjectKeyFromObject(peer)},
		{NamespacedName: client.ObjectKeyFromObject(second)},
	}
	if !reflect.DeepEqual(got, want) || kubeClient.calls != 4 {
		t.Fatalf("later event did not reobserve the parent: requests=%#v, list calls=%d", got, kubeClient.calls)
	}
}

func TestHTTPRouteRequestsForServicePreserveContendersAndDetachedBindings(t *testing.T) {
	cfg := testConfig()
	direct := mapperTestHTTPRoute("default", "direct", gatewayv1.ParentReference{Name: "edge"}, "changed")
	second := mapperTestHTTPRoute("default", "second", gatewayv1.ParentReference{Name: "edge"}, "changed")
	peer := mapperTestHTTPRoute("default", "peer", gatewayv1.ParentReference{Name: "edge"}, "stable")
	other := mapperTestHTTPRoute("default", "other", gatewayv1.ParentReference{Name: "other-edge"}, "changed")
	otherPeer := mapperTestHTTPRoute("default", "other-peer", gatewayv1.ParentReference{Name: "other-edge"}, "stable")
	detaching := mapperTestHTTPRoute("default", "detaching", gatewayv1.ParentReference{Name: "removed"}, "changed")
	(&Reconciler{Config: cfg}).applyRouteBinding(detaching, &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "removed", UID: "old-gateway"}})
	detaching.Annotations[cfg.RouteLifecycleAnnotation()] = controller.RouteDetaching
	detaching.Spec.ParentRefs = nil
	unrelated := mapperTestHTTPRoute("default", "unrelated", gatewayv1.ParentReference{Name: "unrelated"}, "stable")
	base := indexedFakeClientBuilder(testScheme(t), cfg).WithObjects(direct, second, peer, other, otherPeer, detaching, unrelated).Build()
	reader := &fanoutCountingClient{Client: base}
	r := &Reconciler{Client: reader, Config: cfg}
	got := r.enqueueHTTPRoutesForServiceKey(context.Background(), client.ObjectKey{Namespace: "default", Name: "changed"})
	want := []reconcile.Request{
		{NamespacedName: client.ObjectKeyFromObject(detaching)},
		{NamespacedName: client.ObjectKeyFromObject(direct)},
		{NamespacedName: client.ObjectKeyFromObject(other)},
		{NamespacedName: client.ObjectKeyFromObject(otherPeer)},
		{NamespacedName: client.ObjectKeyFromObject(peer)},
		{NamespacedName: client.ObjectKeyFromObject(second)},
	}
	if !reflect.DeepEqual(got, want) || reader.lists != 3 || reader.gets != 0 {
		t.Fatalf("requests=%#v, reads=%d/%d, want all direct and contender routes with three lists", got, reader.gets, reader.lists)
	}
}

func TestHTTPRouteRequestsForServiceIncludeOnlyGatewayContenders(t *testing.T) {
	config := testConfig()
	direct := mapperTestHTTPRoute("default", "direct", gatewayv1.ParentReference{Name: "edge"}, "changed")
	peer := mapperTestHTTPRoute("default", "peer", gatewayv1.ParentReference{Name: "edge"}, "stable")
	unrelated := mapperTestHTTPRoute("default", "unrelated", gatewayv1.ParentReference{Name: "other"}, "stable")

	kubeClient := indexedFakeClientBuilder(testScheme(t), config).
		WithObjects(direct, peer, unrelated).
		Build()
	reconciler := newTestReconciler(Reconciler{Client: kubeClient, Config: config}, nil)

	got := reconciler.enqueueHTTPRoutesForServiceKey(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      "changed",
	})
	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "direct"}},
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "peer"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueueHTTPRoutesForServiceKey() = %#v, want %#v", got, want)
	}
}

func mapperTestHTTPRoute(namespace, name string, parent gatewayv1.ParentReference, service string) *gatewayv1.HTTPRoute {
	port := gatewayv1.PortNumber(80)
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(service),
					Port: &port,
				}},
			}}}},
		},
	}
}

type failNthListClient struct {
	client.Client
	failAt int
	calls  int
	err    error
}

func (c *failNthListClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	c.calls++
	if c.calls == c.failAt {
		return c.err
	}
	return c.Client.List(ctx, list, options...)
}
