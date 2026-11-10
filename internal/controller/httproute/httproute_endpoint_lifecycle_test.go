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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

// endpointOnlyReader makes Node dependency regressions fail even when a model
// build has other legitimate reads for mutation fences and ownership checks.
type endpointOnlyReader struct{ client.Client }

func (r *endpointOnlyReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Node); ok {
		return errors.New("Endpoint mode read a Node")
	}
	return r.Client.Get(ctx, key, object, options...)
}

func (r *endpointOnlyReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, ok := list.(*corev1.NodeList); ok {
		return errors.New("Endpoint mode listed Nodes")
	}
	return r.Client.List(ctx, list, options...)
}

func TestEndpointMembershipChangesPreserveAttachedRoute(t *testing.T) {
	cfg := testConfig()
	class, gateway, route, service, endpointSlice := endpointRouteFixture(t)
	(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway, route).WithObjects(class, gateway, route, service, endpointSlice).Build()
	reader := &endpointOnlyReader{Client: kubeClient}
	provider := &recordingProvider{}
	r := newTestReconciler(Reconciler{Client: reader, APIReader: reader, Config: cfg, Coordinator: &graph.Coordinator{}}, provider)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}
	for _, step := range []struct {
		name      string
		endpoints []discoveryv1.Endpoint
		want      []cloud.Member
	}{
		{name: "ready", endpoints: endpointSlice.Endpoints, want: []cloud.Member{{Address: "192.0.2.20", Port: 8080}}},
		{name: "draining", endpoints: []discoveryv1.Endpoint{{Addresses: []string{"192.0.2.20"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false), Serving: ptr.To(true), Terminating: ptr.To(true)}}}, want: []cloud.Member{{Address: "192.0.2.20", Port: 8080, Draining: true}}},
		{name: "empty"},
		{name: "replacement", endpoints: []discoveryv1.Endpoint{{Addresses: []string{"192.0.2.21"}}}, want: []cloud.Member{{Address: "192.0.2.21", Port: 8080}}},
	} {
		t.Run(step.name, func(t *testing.T) {
			var currentSlice discoveryv1.EndpointSlice
			if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(endpointSlice), &currentSlice); err != nil {
				t.Fatal(err)
			}
			currentSlice.Endpoints = step.endpoints
			if err := kubeClient.Update(t.Context(), &currentSlice); err != nil {
				t.Fatal(err)
			}
			before := len(provider.routeSpecs)
			if _, err := r.Reconcile(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if len(provider.routeSpecs) != before+1 || len(provider.deletedRoutes) != 0 {
				t.Fatalf("route was detached or not ensured: ensures=%d deletes=%d", len(provider.routeSpecs), len(provider.deletedRoutes))
			}
			desired := provider.routeSpecs[before]
			if desired.MemberMode != cloud.MemberModeEndpoint || !slices.Equal(desired.Members, step.want) {
				t.Fatalf("desired = %#v, want members %#v", desired, step.want)
			}
			var current gatewayv1.HTTPRoute
			if err := kubeClient.Get(t.Context(), request.NamespacedName, &current); err != nil {
				t.Fatal(err)
			}
			if current.Annotations[cfg.RouteLifecycleAnnotation()] != controller.RouteAttached || len(current.Status.Parents) != 1 ||
				!meta.IsStatusConditionTrue(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs)) ||
				!meta.IsStatusConditionTrue(current.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed") {
				t.Fatalf("membership change altered route attachment: %#v", current)
			}
			snapshot, err := r.buildRouteCleanupSnapshot(t.Context(), &current, desired.Identity)
			if err != nil || snapshot.disposition != routeCleanupStillDesired {
				t.Fatalf("live cleanup check discarded an Endpoint graph: %#v, %v", snapshot, err)
			}
		})
	}
}

func TestEndpointSelectionUsesFrozenGatewayMode(t *testing.T) {
	cfg := testConfig()
	_, gateway, older, service, endpointSlice := endpointRouteFixture(t)
	older.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	newer := older.DeepCopy()
	newer.Name, newer.UID = "newer", "newer-uid"
	newer.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	newer.Spec.Rules[0].BackendRefs[0].Name = "node-port"
	nodePort, _, _ := validNodePortBackend(gateway.Namespace, "node-port")
	// The live class defaults to NodePort. Both route decisions use only the
	// already frozen Endpoint mode on this Gateway.
	reader := indexedFakeClientBuilder(testScheme(t), cfg).WithObjects(older, newer, service, endpointSlice, nodePort).Build()
	r := &Reconciler{Config: cfg, Client: reader}
	parent := managedRouteParent{ref: newer.Spec.ParentRefs[0], gateway: gateway, listener: &gateway.Spec.Listeners[0]}
	selected, err := r.isSelectedRoute(t.Context(), newer, parent)
	if err != nil || selected {
		t.Fatalf("older valid ClusterIP route lost its Endpoint slot: selected=%t, error=%v", selected, err)
	}
	parent.ref = older.Spec.ParentRefs[0]
	selected, err = r.isSelectedRoute(t.Context(), older, parent)
	if err != nil || !selected {
		t.Fatalf("frozen Endpoint route was rejected: selected=%t, error=%v", selected, err)
	}
}

func TestEndpointServiceMutationFence(t *testing.T) {
	for _, change := range []string{"replaced", "LoadBalancer", "port"} {
		t.Run(change, func(t *testing.T) {
			cfg := testConfig()
			class, gateway, route, service, endpointSlice := endpointRouteFixture(t)
			(&Reconciler{Config: cfg}).applyRouteBinding(route, gateway)
			reader := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway, route).WithObjects(class, gateway, route, service, endpointSlice).Build()
			r := &Reconciler{Config: cfg, Client: reader, APIReader: reader}
			snapshot, err := r.buildRouteValidationSnapshot(t.Context(), route, gateway, routeSnapshotForEnsure)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.sealRouteMutationSnapshot(t.Context(), snapshot, routeBindingRequired); err != nil {
				t.Fatalf("unchanged Service fence failed: %v", err)
			}
			var current corev1.Service
			if err := reader.Get(t.Context(), client.ObjectKeyFromObject(service), &current); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "replaced":
				if err := reader.Delete(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
				current.UID, current.ResourceVersion = "replacement-service", ""
				if err := reader.Create(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
			case "LoadBalancer":
				current.Spec.Type = corev1.ServiceTypeLoadBalancer
				if err := reader.Update(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
			case "port":
				current.Spec.Ports[0].Port = 81
				if err := reader.Update(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.sealRouteMutationSnapshot(t.Context(), snapshot, routeBindingRequired); !errors.Is(err, errHTTPRouteChanged) {
				t.Fatalf("changed backend passed mutation fence: %v", err)
			}
		})
	}
}
