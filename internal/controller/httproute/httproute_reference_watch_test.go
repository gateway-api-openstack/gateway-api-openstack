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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

func TestReferenceGrantEventsSelectDeclaredServiceConsumers(t *testing.T) {
	parent := gatewayv1.ParentReference{Name: "edge", Namespace: ptr.To(gatewayv1.Namespace("gateways"))}
	direct := mapperTestHTTPRoute("apps", "direct", parent, "stable")
	direct.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("backends"))
	direct.Spec.ParentRefs = append(direct.Spec.ParentRefs, gatewayv1.ParentReference{Name: "second", Namespace: parent.Namespace})
	// The target appears in another rule, not just the first backend slot.
	direct.Spec.Rules = append(direct.Spec.Rules, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: "canary", Namespace: ptr.To(gatewayv1.Namespace("backends")), Port: ptr.To(gatewayv1.PortNumber(80))},
	}}}})
	peer := mapperTestHTTPRoute("other", "peer", parent, "unrelated")
	secondPeer := mapperTestHTTPRoute("other", "second-peer", direct.Spec.ParentRefs[1], "unrelated")
	otherService := mapperTestHTTPRoute("apps", "other-service", gatewayv1.ParentReference{Name: "other"}, "other")
	otherService.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("backends"))
	wrongSource := mapperTestHTTPRoute("foreign", "wrong-source", gatewayv1.ParentReference{Name: "foreign"}, "canary")
	wrongSource.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("backends"))
	wrongKind := mapperTestHTTPRoute("apps", "wrong-kind", gatewayv1.ParentReference{Name: "foreign"}, "canary")
	wrongKind.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("backends"))
	wrongKind.Spec.Rules[0].BackendRefs[0].Kind = ptr.To(gatewayv1.Kind("Secret"))
	sameNamespace := mapperTestHTTPRoute("backends", "same-namespace", gatewayv1.ParentReference{Name: "local"}, "canary")
	config := testConfig()
	base := indexedFakeClientBuilder(testScheme(t), config).WithObjects(direct, peer, secondPeer, otherService, wrongSource, wrongKind, sameNamespace).Build()
	grant := serviceReferenceGrant("backends", "apps", "canary")
	for _, test := range []struct {
		name   string
		mutate func(*gatewayv1.ReferenceGrant)
		want   []*gatewayv1.HTTPRoute
	}{
		{name: "exact second rule", want: []*gatewayv1.HTTPRoute{direct, peer, secondPeer}},
		{name: "all Service names", mutate: func(g *gatewayv1.ReferenceGrant) { g.Spec.To[0].Name = nil }, want: []*gatewayv1.HTTPRoute{direct, peer, secondPeer, otherService}},
		{name: "unrelated target namespace", mutate: func(g *gatewayv1.ReferenceGrant) { g.Namespace = "elsewhere" }},
		{name: "unrelated source namespace", mutate: func(g *gatewayv1.ReferenceGrant) { g.Spec.From[0].Namespace = "elsewhere" }},
		{name: "unsupported source kind", mutate: func(g *gatewayv1.ReferenceGrant) { g.Spec.From[0].Kind = "Gateway" }},
		{name: "unsupported target kind", mutate: func(g *gatewayv1.ReferenceGrant) { g.Spec.To[0].Kind = "Secret" }},
		{name: "same namespace needs no grant", mutate: func(g *gatewayv1.ReferenceGrant) { g.Spec.From[0].Namespace = "backends" }},
		{name: "duplicate grant entries", mutate: func(g *gatewayv1.ReferenceGrant) {
			g.Spec.From = append(g.Spec.From, g.Spec.From[0])
			g.Spec.To = append(g.Spec.To, g.Spec.To[0])
		}, want: []*gatewayv1.HTTPRoute{direct, peer, secondPeer}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := grant.DeepCopy()
			if test.mutate != nil {
				test.mutate(candidate)
			}
			reader := &referenceWatchClient{Client: base, t: t}
			r := &Reconciler{Client: reader, Config: config}
			assertRouteRequests(t, r.enqueueHTTPRoutesForReferenceGrant(t.Context(), candidate), test.want...)
		})
	}
}

func TestReferenceGrantUpdateAndDeletionReachOldConsumers(t *testing.T) {
	oldRoute := mapperTestHTTPRoute("old-source", "old", gatewayv1.ParentReference{Name: "old"}, "old-service")
	oldRoute.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("backends"))
	newRoute := mapperTestHTTPRoute("new-source", "new", gatewayv1.ParentReference{Name: "new"}, "new-service")
	newRoute.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("backends"))
	config := testConfig()
	r := &Reconciler{Client: indexedFakeClientBuilder(testScheme(t), config).WithObjects(oldRoute, newRoute).Build(), Config: config}
	before := serviceReferenceGrant("backends", "old-source", "old-service")
	after := serviceReferenceGrant("backends", "new-source", "new-service")
	after.Generation = before.Generation + 1
	update := event.UpdateEvent{ObjectOld: before, ObjectNew: after}
	if !controller.GenerationOrDeletionChangedPredicate().Update(update) {
		t.Fatal("grant update was filtered")
	}
	queue := newReferenceWatchQueue(t)
	mapped := handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutesForReferenceGrant)
	mapped.Update(t.Context(), update, queue)
	assertRouteRequests(t, drainReferenceWatchQueue(queue), oldRoute, newRoute)
	mapped.Delete(t.Context(), event.DeleteEvent{Object: after}, queue)
	assertRouteRequests(t, drainReferenceWatchQueue(queue), newRoute)
	mapped.Create(t.Context(), event.CreateEvent{Object: before}, queue)
	assertRouteRequests(t, drainReferenceWatchQueue(queue), oldRoute)
}

func TestNamespaceEventsFollowOnlySelectorParents(t *testing.T) {
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gateways", Name: "edge"}, Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{
		{Name: "selected", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
			From: ptr.To(gatewayv1.NamespacesFromSelector), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "blue"}},
		}}},
		{Name: "all", Port: 8080, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)}}},
	}}}
	parent := gatewayv1.ParentReference{Name: "edge", Namespace: ptr.To(gatewayv1.Namespace("gateways")), SectionName: ptr.To(gatewayv1.SectionName("selected"))}
	direct := mapperTestHTTPRoute("apps", "direct", parent, "backend")
	peer := mapperTestHTTPRoute("other", "peer", parent, "backend")
	allGateway := gateway.DeepCopy()
	allGateway.Name = "all"
	allGateway.Spec.Listeners = gateway.Spec.Listeners[1:]
	unrelated := mapperTestHTTPRoute("apps", "unrelated", gatewayv1.ParentReference{Name: "all", Namespace: parent.Namespace}, "backend")
	missing := mapperTestHTTPRoute("apps", "missing", gatewayv1.ParentReference{Name: "missing"}, "backend")
	config := testConfig()
	base := indexedFakeClientBuilder(testScheme(t), config).WithObjects(gateway, allGateway, direct, peer, unrelated, missing).Build()
	r := &Reconciler{Client: &referenceWatchClient{Client: base, t: t}, Config: config}
	before := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps", Labels: map[string]string{"team": "blue"}}}
	after := before.DeepCopy()
	after.Labels["team"] = "red"
	update := event.UpdateEvent{ObjectOld: before, ObjectNew: after}
	if !controller.NamespacePredicate().Update(update) {
		t.Fatal("namespace label update was filtered")
	}
	queue := newReferenceWatchQueue(t)
	handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutesForNamespace).Update(t.Context(), update, queue)
	assertRouteRequests(t, drainReferenceWatchQueue(queue), direct, peer)
	if parentUsesNamespaceSelector(gateway, gatewayv1.ParentReference{SectionName: ptr.To(gatewayv1.SectionName("all"))}) {
		t.Fatal("unrelated listener was treated as a selector dependency")
	}
	if parentUsesNamespaceSelector(gateway, gatewayv1.ParentReference{Port: ptr.To(gatewayv1.PortNumber(8080))}) {
		t.Fatal("unrelated listener port was treated as a selector dependency")
	}
}

func TestEndpointSliceServiceChangeAndServiceRecreationNotifyConsumers(t *testing.T) {
	oldRoute := mapperTestHTTPRoute("apps", "old", gatewayv1.ParentReference{Name: "old"}, "old-service")
	newRoute := mapperTestHTTPRoute("apps", "new", gatewayv1.ParentReference{Name: "new"}, "new-service")
	unrelated := mapperTestHTTPRoute("other", "unrelated", gatewayv1.ParentReference{Name: "unrelated"}, "old-service")
	config := testConfig()
	r := &Reconciler{Client: indexedFakeClientBuilder(testScheme(t), config).WithObjects(oldRoute, newRoute, unrelated).Build(), Config: config}
	before := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "endpoints", Labels: map[string]string{discoveryv1.LabelServiceName: "old-service"}}}
	after := before.DeepCopy()
	after.Labels[discoveryv1.LabelServiceName] = "new-service"
	update := event.UpdateEvent{ObjectOld: before, ObjectNew: after}
	if !controller.EndpointSlicePredicate().Update(update) {
		t.Fatal("Service label reassignment was filtered")
	}
	queue := newReferenceWatchQueue(t)
	handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutesForEndpointSlice).Update(t.Context(), update, queue)
	assertRouteRequests(t, drainReferenceWatchQueue(queue), oldRoute, newRoute)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "old-service", UID: "old-uid"}}
	mapped := handler.EnqueueRequestsFromMapFunc(r.enqueueHTTPRoutesForService)
	mapped.Delete(t.Context(), event.DeleteEvent{Object: service}, queue)
	assertRouteRequests(t, drainReferenceWatchQueue(queue), oldRoute)
	service.UID = "new-uid"
	mapped.Create(t.Context(), event.CreateEvent{Object: service}, queue)
	assertRouteRequests(t, drainReferenceWatchQueue(queue), oldRoute)
}

func serviceReferenceGrant(namespace, source, service string) *gatewayv1.ReferenceGrant {
	return &gatewayv1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "grant", Generation: 1}, Spec: gatewayv1.ReferenceGrantSpec{
		From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: gatewayv1.Namespace(source)}},
		To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1.ObjectName(service))}},
	}}
}

// Reject accidental cluster scans in dependency mappers. A namespace-only
// route list uses the cache's built-in namespace index.
type referenceWatchClient struct {
	client.Client
	t *testing.T
}

func (c *referenceWatchClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	opts := &client.ListOptions{}
	opts.ApplyOptions(options)
	if opts.Namespace == "" && (opts.FieldSelector == nil || opts.FieldSelector.Empty()) {
		c.t.Fatalf("unscoped dependency list for %T", list)
	}
	return c.Client.List(ctx, list, options...)
}

func newReferenceWatchQueue(t *testing.T) workqueue.TypedRateLimitingInterface[reconcile.Request] {
	t.Helper()
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	t.Cleanup(queue.ShutDown)
	return queue
}

func drainReferenceWatchQueue(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) []reconcile.Request {
	keys := make(map[types.NamespacedName]struct{})
	for queue.Len() != 0 {
		request, _ := queue.Get()
		keys[request.NamespacedName] = struct{}{}
		queue.Done(request)
	}
	return controller.SortedRequests(keys)
}

func assertRouteRequests(t *testing.T, got []reconcile.Request, routes ...*gatewayv1.HTTPRoute) {
	t.Helper()
	keys := make(map[types.NamespacedName]struct{})
	for _, route := range routes {
		keys[client.ObjectKeyFromObject(route)] = struct{}{}
	}
	want := controller.SortedRequests(keys)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
}
