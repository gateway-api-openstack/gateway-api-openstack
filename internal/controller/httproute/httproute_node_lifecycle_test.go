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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// These contract checks use fake storage and the production event mapper and
// graph writer. They do not change real Nodes or establish network reachability.
func TestNodeLifecyclePreservesUnchangedMembers(t *testing.T) {
	for _, policy := range []corev1.ServiceExternalTrafficPolicy{
		corev1.ServiceExternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyLocal,
	} {
		t.Run(string(policy), func(t *testing.T) {
			fixture := newMemberBurstFixture(t, 1)
			var service corev1.Service
			if err := fixture.reader.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "backend"}, &service); err != nil {
				t.Fatal(err)
			}
			service.Spec.ExternalTrafficPolicy = policy
			if err := fixture.reader.Update(t.Context(), &service); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []struct {
				name             string
				members, changes int
			}{
				{name: "cordon", members: 1, changes: 1},
				{name: "uncordon", members: 2, changes: 1},
				{name: "address", members: 2, changes: 2},
				{name: "delete", members: 1, changes: 1},
				{name: "create", members: 2, changes: 1},
			} {
				t.Run(phase.name, func(t *testing.T) {
					queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
					defer queue.ShutDown()
					mapped := handler.EnqueueRequestsFromMapFunc(fixture.reconciler.enqueueHTTPRoutesForNode)
					predicate := controller.NodePredicate("")
					previous := fixture.node.DeepCopy()
					switch phase.name {
					case "delete":
						if err := fixture.reader.Delete(t.Context(), fixture.node); err != nil {
							t.Fatal(err)
						}
						deletion := event.DeleteEvent{Object: previous}
						if !predicate.Delete(deletion) {
							t.Fatal("Node deletion was ignored")
						}
						mapped.Delete(t.Context(), deletion, queue)
					case "create":
						fixture.node.ResourceVersion = ""
						if err := fixture.reader.Create(t.Context(), fixture.node); err != nil {
							t.Fatal(err)
						}
						creation := event.CreateEvent{Object: fixture.node.DeepCopy()}
						if !predicate.Create(creation) {
							t.Fatal("Node creation was ignored")
						}
						mapped.Create(t.Context(), creation, queue)
					default:
						switch phase.name {
						case "cordon", "uncordon":
							fixture.node.Spec.Unschedulable = phase.name == "cordon"
							if err := fixture.reader.Update(t.Context(), fixture.node); err != nil {
								t.Fatal(err)
							}
						case "address":
							fixture.node.Status.Addresses[0].Address = "192.0.2.12"
							if err := fixture.reader.Status().Update(t.Context(), fixture.node); err != nil {
								t.Fatal(err)
							}
						}
						update := event.UpdateEvent{ObjectOld: previous, ObjectNew: fixture.node.DeepCopy()}
						if !predicate.Update(update) {
							t.Fatal("Node dependency transition was ignored")
						}
						mapped.Update(t.Context(), update, queue)
					}
					fixture.verifyNodeLifecycleQueue(t, queue, phase.members, phase.changes)
				})
			}
		})
	}
}

func (f *memberBurstFixture) verifyNodeLifecycleQueue(t *testing.T, queue workqueue.TypedRateLimitingInterface[reconcile.Request], members, changes int) {
	t.Helper()
	if queue.Len() != len(f.routes) {
		t.Fatalf("queued %d routes, want %d", queue.Len(), len(f.routes))
	}
	f.provider.calls, f.provider.changes = 0, 0
	for queue.Len() != 0 {
		request, shutdown := queue.Get()
		if shutdown || f.provider.calls > changes {
			t.Fatal("Node transition exceeded the expected reconcile budget")
		}
		result, err := f.reconciler.Reconcile(t.Context(), request)
		queue.Done(request)
		if err != nil {
			t.Fatal(err)
		}
		resync := controller.OpenStackResyncAfter(f.reconciler.Config.OpenStackResyncInterval, f.routes[request.NamespacedName])
		if result.RequeueAfter > 0 && result.RequeueAfter != resync {
			queue.Add(request)
		}
	}
	if f.provider.calls != changes+1 || f.provider.changes != changes {
		t.Fatalf("provider calls=%d changes=%d, want %d/%d", f.provider.calls, f.provider.changes, changes+1, changes)
	}
	for _, uid := range f.routes {
		current := f.provider.members[string(uid)]
		if len(current) != members || current[f.unchanged] != "unchanged-member" {
			t.Fatalf("Node transition replaced an unchanged member or has the wrong set: %#v", current)
		}
	}
}
