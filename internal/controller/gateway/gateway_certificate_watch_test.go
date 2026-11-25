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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestCertificateEventsReachOnlyManagedReferences(t *testing.T) {
	gateway, class, grant, secret := listenerInputFixtures()
	unrelated := gateway.DeepCopy()
	unrelated.Name = "unrelated"
	unrelated.Spec.Listeners[0].TLS.CertificateRefs[0].Name = "another"
	foreign := gateway.DeepCopy()
	foreign.Name = "foreign"
	foreign.Spec.GatewayClassName = "foreign"
	foreignClass := class.DeepCopy()
	foreignClass.Name = "foreign"
	foreignClass.Spec.ControllerName = "foreign.example/controller"
	kubeClient := rawIndexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(gateway, class, unrelated, foreign, foreignClass).Build()
	reconciler := &Reconciler{Client: kubeClient, Config: testConfig()}
	want := []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(gateway)}}
	metadata := &metav1.PartialObjectMetadata{ObjectMeta: secret.ObjectMeta}
	if got := reconciler.gatewayRequestsForCertificateSecret(t.Context(), metadata); !reflect.DeepEqual(got, want) {
		t.Fatalf("Secret requests=%v, want %v", got, want)
	}
	if got := reconciler.gatewayRequestsForCertificateGrant(t.Context(), grant); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReferenceGrant requests=%v, want %v", got, want)
	}
	deleting := grant.DeepCopy()
	deleting.DeletionTimestamp = ptr.To(metav1.Now())
	if got := reconciler.gatewayRequestsForCertificateGrant(t.Context(), deleting); !reflect.DeepEqual(got, want) {
		t.Fatalf("deleting ReferenceGrant requests=%v, want %v", got, want)
	}
	for _, change := range []func(*gatewayv1.ReferenceGrant){
		func(grant *gatewayv1.ReferenceGrant) { grant.Spec.From[0].Namespace = "different" },
		func(grant *gatewayv1.ReferenceGrant) { grant.Spec.From[0].Kind = "HTTPRoute" },
		func(grant *gatewayv1.ReferenceGrant) { grant.Spec.To[0].Kind = "Service" },
		func(grant *gatewayv1.ReferenceGrant) { grant.Spec.To[0].Name = ptr.To(gatewayv1.ObjectName("unused")) },
	} {
		irrelevant := grant.DeepCopy()
		change(irrelevant)
		if got := reconciler.gatewayRequestsForCertificateGrant(t.Context(), irrelevant); len(got) != 0 {
			t.Fatalf("unrelated grant enqueued Gateways: %v", got)
		}
	}

	// The handler maps both revisions, so removing the final permission still
	// reaches the Gateway whose old reference was authorized.
	revoked := grant.DeepCopy()
	revoked.Spec.To[0].Name = ptr.To(gatewayv1.ObjectName("unused"))
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()
	handler.EnqueueRequestsFromMapFunc(reconciler.gatewayRequestsForCertificateGrant).Update(t.Context(), event.UpdateEvent{ObjectOld: grant, ObjectNew: revoked}, queue)
	if queue.Len() != 1 {
		t.Fatalf("revocation queue length=%d, want one affected Gateway", queue.Len())
	}
	request, _ := queue.Get()
	queue.Done(request)
	if request != want[0] {
		t.Fatalf("revocation request=%v, want %v", request, want[0])
	}
}
