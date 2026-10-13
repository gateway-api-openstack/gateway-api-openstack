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
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

// TestDependencyBurstsConvergeMembers runs the production predicates, mapped
// update handler, HTTPRoute reconciler, and graph writer. Kubernetes storage and
// the provider are fakes. Progress requeues run immediately, and slow resyncs do
// not run, so this measures local work rather than manager or cloud latency.
func TestDependencyBurstsConvergeMembers(t *testing.T) {
	for _, source := range []string{"Node", "EndpointSlice"} {
		for _, count := range []int{1, 8} {
			t.Run(fmt.Sprintf("%s/routes=%d", source, count), func(t *testing.T) {
				fixture := newMemberBurstFixture(t, count)
				for _, phase := range []struct {
					name        string
					events      int
					wantMembers int
					wantChanges int
				}{
					{name: "remove", events: 5, wantMembers: 1, wantChanges: count},
					{name: "unchanged", events: 6, wantMembers: 1},
					{name: "restore", events: 5, wantMembers: 2, wantChanges: count},
				} {
					t.Run(phase.name, func(t *testing.T) {
						fixture.runBurst(t, source, phase.events, phase.wantMembers, phase.wantChanges)
					})
				}
			})
		}
	}
}

type memberBurstFixture struct {
	reconciler *Reconciler
	reader     *fanoutCountingClient
	provider   *memberSetProvider
	node       *corev1.Node
	endpoints  *discoveryv1.EndpointSlice
	routes     map[client.ObjectKey]types.UID
	unchanged  cloud.Member
}

func newMemberBurstFixture(t *testing.T, count int) *memberBurstFixture {
	t.Helper()
	cfg := testConfig()
	class, _ := providerRouteParentObjects(cfg)
	service, endpoints, firstNode := validNodePortBackend("default", "backend")
	service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	secondNode := firstNode.DeepCopy()
	secondNode.Name = "worker-1"
	secondNode.Status.Addresses[0].Address = "192.0.2.11"
	endpoints.Endpoints = append(endpoints.Endpoints, discoveryv1.Endpoint{
		NodeName: ptr.To(secondNode.Name), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
	})
	objects := []client.Object{class, service, endpoints, firstNode, secondNode}
	provider := &memberSetProvider{members: map[string]map[cloud.Member]string{}}
	fixture := &memberBurstFixture{
		provider: provider, node: secondNode, endpoints: endpoints, routes: map[client.ObjectKey]types.UID{},
		unchanged: cloud.Member{Address: firstNode.Status.Addresses[0].Address, Port: 30080},
	}
	for index := range count {
		gateway := programmedTestGateway(cfg)
		gateway.Name = fmt.Sprintf("edge-%02d", index)
		gateway.UID = types.UID(gateway.Name)
		route := mapperTestHTTPRoute("default", fmt.Sprintf("route-%02d", index), gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, service.Name)
		route.UID, route.Generation = types.UID(route.Name), 1
		(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
		fixture.routes[client.ObjectKeyFromObject(route)] = route.UID
		provider.members[string(route.UID)] = map[cloud.Member]string{
			fixture.unchanged: "unchanged-member",
			{Address: secondNode.Status.Addresses[0].Address, Port: 30080}: "changing-member",
		}
		objects = append(objects, gateway, route)
	}
	base := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(&gatewayv1.HTTPRoute{}).WithObjects(objects...).Build()
	fixture.reader = &fanoutCountingClient{Client: base}
	fixture.reconciler = newTestReconciler(Reconciler{
		Client: fixture.reader, APIReader: fixture.reader, Coordinator: &graph.Coordinator{}, Config: cfg,
	}, provider)
	for _, request := range controller.SortedRequests(routeKeySet(fixture.routes)) {
		if _, err := fixture.reconciler.Reconcile(t.Context(), request); err != nil {
			t.Fatalf("establish initial route status: %v", err)
		}
	}
	if provider.changes != 0 || provider.calls != count {
		t.Fatalf("initial graph did not converge without changes: calls=%d, changes=%d", provider.calls, provider.changes)
	}
	return fixture
}

func routeKeySet(routes map[client.ObjectKey]types.UID) map[types.NamespacedName]struct{} {
	keys := make(map[types.NamespacedName]struct{}, len(routes))
	for key := range routes {
		keys[key] = struct{}{}
	}
	return keys
}

func (f *memberBurstFixture) runBurst(t *testing.T, source string, events, wantMembers, wantChanges int) {
	t.Helper()
	r := f.reconciler
	f.reader.gets, f.reader.lists = 0, 0
	f.provider.calls, f.provider.changes = 0, 0
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()
	mapper := r.enqueueHTTPRoutesForNode
	predicate := controller.NodePredicate("")
	if source == "EndpointSlice" {
		mapper = r.enqueueHTTPRoutesForEndpointSlice
		predicate = controller.EndpointSlicePredicate()
	}
	enqueues := 0
	mapped := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
		requests := mapper(ctx, object)
		enqueues += len(requests)
		return requests
	})
	for range events {
		update := f.toggleReadiness(t, source)
		if !predicate.Update(update) {
			t.Fatal("dependency predicate ignored a readiness transition")
		}
		mapped.Update(t.Context(), update, queue)
	}
	count := len(f.routes)
	if enqueues != 2*events*count || queue.Len() != count {
		t.Fatalf("mapped requests=%d, queued=%d, want %d/%d", enqueues, queue.Len(), 2*events*count, count)
	}
	mapGets, mapLists := f.reader.gets, f.reader.lists
	if source == "Node" {
		if mapGets > 2*events*(2*count+1) || mapLists > 4*events {
			t.Fatalf("Node mapping exceeded shared dependency budget: gets=%d, lists=%d", mapGets, mapLists)
		}
	} else if mapGets != 0 || mapLists != 2*events*(count+1) {
		t.Fatalf("EndpointSlice mapping exceeded one list per parent: gets=%d, lists=%d", mapGets, mapLists)
	}
	reconciles := 0
	for queue.Len() != 0 {
		request, shutdown := queue.Get()
		if shutdown || reconciles >= 3*count {
			t.Fatal("event burst did not converge within the reconcile budget")
		}
		result, err := r.Reconcile(t.Context(), request)
		queue.Done(request)
		if err != nil {
			t.Fatalf("reconcile queued dependency: %v", err)
		}
		reconciles++
		slowResync := controller.OpenStackResyncAfter(r.Config.OpenStackResyncInterval, f.routes[request.NamespacedName])
		if result.RequeueAfter > 0 && result.RequeueAfter != slowResync {
			queue.Add(request)
		}
	}
	wantReconciles := count + wantChanges
	if reconciles != wantReconciles || f.provider.calls != wantReconciles || f.provider.changes != wantChanges {
		t.Fatalf("reconciles=%d, provider calls=%d, member changes=%d, want %d/%d/%d", reconciles, f.provider.calls, f.provider.changes, wantReconciles, wantReconciles, wantChanges)
	}
	reconcileGets, reconcileLists := f.reader.gets-mapGets, f.reader.lists-mapLists
	// The complete live fences currently use 25 Get and 11 List calls per
	// reconcile. Keep a small ceiling above that baseline as the fixture grows.
	if reconcileGets > reconciles*30 || reconcileLists > reconciles*12 {
		t.Fatalf("reconciliation exceeded read budget: gets=%d, lists=%d, reconciles=%d", reconcileGets, reconcileLists, reconciles)
	}
	for key, uid := range f.routes {
		members := f.provider.members[string(uid)]
		if len(members) != wantMembers || members[f.unchanged] != "unchanged-member" {
			t.Fatalf("route %s changed its stable member or did not converge: %#v", key, members)
		}
		var route gatewayv1.HTTPRoute
		if err := f.reader.Client.Get(t.Context(), key, &route); err != nil {
			t.Fatal(err)
		}
		if len(route.Status.Parents) != 1 || !meta.IsStatusConditionTrue(route.Status.Parents[0].Conditions, r.Config.Domain()+"/Programmed") {
			t.Fatalf("route %s did not report Programmed after convergence: %#v", key, route.Status)
		}
	}
	t.Logf("events=%d mapped=%d reconciles=%d provider_calls=%d member_changes=%d mapper_gets=%d mapper_lists=%d reconcile_gets=%d reconcile_lists=%d", events, enqueues, reconciles, f.provider.calls, f.provider.changes, mapGets, mapLists, reconcileGets, reconcileLists)
}

func (f *memberBurstFixture) toggleReadiness(t *testing.T, source string) event.UpdateEvent {
	t.Helper()
	if source == "Node" {
		previous := f.node.DeepCopy()
		if f.node.Status.Conditions[0].Status == corev1.ConditionTrue {
			f.node.Status.Conditions[0].Status = corev1.ConditionFalse
		} else {
			f.node.Status.Conditions[0].Status = corev1.ConditionTrue
		}
		if err := f.reader.Client.Status().Update(t.Context(), f.node); err != nil {
			t.Fatal(err)
		}
		return event.UpdateEvent{ObjectOld: previous, ObjectNew: f.node.DeepCopy()}
	}
	previous := f.endpoints.DeepCopy()
	f.endpoints.Endpoints[1].Conditions.Ready = ptr.To(!*f.endpoints.Endpoints[1].Conditions.Ready)
	if err := f.reader.Update(t.Context(), f.endpoints); err != nil {
		t.Fatal(err)
	}
	return event.UpdateEvent{ObjectOld: previous, ObjectNew: f.endpoints.DeepCopy()}
}

// memberSetProvider is a stateful test double for the cloud boundary. It applies
// one member change per call and reports progress until a later observation.
// It does not simulate Octavia ownership checks, HTTP requests, or timing.
type memberSetProvider struct {
	members        map[string]map[cloud.Member]string
	calls, changes int
}

func (p *memberSetProvider) ReconcileGatewayGraph(_ context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	p.calls++
	if spec.Gateway.Disposition != cloud.GraphEnsure || len(spec.Routes) != 1 || spec.Routes[0].Disposition != cloud.GraphEnsure {
		return cloud.GatewayGraphResult{}, fmt.Errorf("expected one complete ensured Gateway and route")
	}
	route := spec.Routes[0].Spec
	members := p.members[route.Identity.RouteUID]
	if members == nil {
		return cloud.GatewayGraphResult{}, fmt.Errorf("unexpected route identity")
	}
	changed := false
	for member := range members {
		if !slices.Contains(route.Members, member) {
			delete(members, member)
			changed = true
			break
		}
	}
	if !changed {
		for _, member := range route.Members {
			if _, present := members[member]; !present {
				members[member] = "replacement-member"
				changed = true
				break
			}
		}
	}
	outcome := cloud.ReadyOutcome()
	if changed {
		p.changes++
		outcome = cloud.ProgressingOutcome("Member update in progress", time.Second)
	}
	return cloud.GatewayGraphResult{
		Outcome: outcome,
		Gateway: cloud.GatewayReadyResult(cloud.GatewayState{LoadBalancerID: "lb-" + spec.Identity.GatewayUID, ListenerID: "listener-" + spec.Identity.GatewayUID}),
		Routes: []cloud.RouteGraphResult{{Identity: route.Identity, Result: cloud.RouteResult{
			Outcome: outcome, State: cloud.RouteState{PoolID: "pool-" + route.Identity.RouteUID},
		}}},
	}, nil
}
