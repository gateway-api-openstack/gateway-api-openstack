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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestNodeMapperReobservesDependenciesBetweenEvents(t *testing.T) {
	const count = 3
	r, reader, node := nodeFanoutFixture(t, count, corev1.ServiceExternalTrafficPolicyLocal)
	ctx := context.Background()
	check := func(want int) {
		t.Helper()
		if got := len(r.enqueueHTTPRoutesForNode(ctx, node)); got != want {
			t.Fatalf("enqueued routes = %d, want %d", got, want)
		}
	}
	check(count)
	var endpoint discoveryv1.EndpointSlice
	if err := reader.Get(ctx, client.ObjectKey{Namespace: "default", Name: "backend-1"}, &endpoint); err != nil {
		t.Fatal(err)
	}
	endpoint.Endpoints[0].Conditions.Ready = ptr.To(false)
	if err := reader.Update(ctx, &endpoint); err != nil {
		t.Fatal(err)
	}
	check(0)
	endpoint.Endpoints[0].Conditions.Ready = ptr.To(true)
	endpoint.Endpoints[0].NodeName = ptr.To("other-worker")
	if err := reader.Update(ctx, &endpoint); err != nil {
		t.Fatal(err)
	}
	check(0)
	endpoint.Endpoints[0].NodeName = ptr.To(node.Name)
	if err := reader.Update(ctx, &endpoint); err != nil {
		t.Fatal(err)
	}
	check(count)
	var service corev1.Service
	if err := reader.Get(ctx, client.ObjectKey{Namespace: "default", Name: "backend"}, &service); err != nil {
		t.Fatal(err)
	}
	service.Spec.Type = corev1.ServiceTypeClusterIP
	if err := reader.Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
	check(0)
	service.Spec.Type = corev1.ServiceTypeNodePort
	if err := reader.Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
	check(count)
	var class gatewayv1.GatewayClass
	if err := reader.Get(ctx, client.ObjectKey{Name: "managed"}, &class); err != nil {
		t.Fatal(err)
	}
	class.Spec.ControllerName = "other.example.com/controller"
	if err := reader.Update(ctx, &class); err != nil {
		t.Fatal(err)
	}
	check(0)
}

func TestEndpointDeletionStillEnqueuesLocalBackends(t *testing.T) {
	const count = 3
	r, reader, node := nodeFanoutFixture(t, count, corev1.ServiceExternalTrafficPolicyLocal)
	ctx := context.Background()
	if got := len(r.enqueueHTTPRoutesForNode(ctx, node)); got != count {
		t.Fatalf("initial Node requests = %d, want %d", got, count)
	}
	var endpoint discoveryv1.EndpointSlice
	if err := reader.Get(ctx, client.ObjectKey{Namespace: "default", Name: "backend-1"}, &endpoint); err != nil {
		t.Fatal(err)
	}
	if err := reader.Delete(ctx, &endpoint); err != nil {
		t.Fatal(err)
	}
	if got := len(r.enqueueHTTPRoutesForNode(ctx, node)); got != 0 {
		t.Fatalf("Node event retained deleted local endpoints: %d routes", got)
	}
	requests := r.enqueueHTTPRoutesForEndpointSlice(ctx, &endpoint)
	names := make(map[string]bool, len(requests))
	for _, request := range requests {
		names[request.Name] = true
	}
	for _, name := range []string{"route-0000", "route-0001", "route-0002"} {
		if !names[name] {
			t.Errorf("EndpointSlice deletion did not enqueue %s", name)
		}
	}
}
