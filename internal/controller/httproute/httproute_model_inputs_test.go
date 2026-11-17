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
	"fmt"
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestRouteGraphSharesBackendObservationAcrossCandidates(t *testing.T) {
	f := newRouteModelInputFixture(t)
	unsupported := f.candidate("a-unsupported")
	unsupported.Spec.Rules[0].BackendRefs[0].Weight = ptr.To(int32(80))
	other := *unsupported.Spec.Rules[0].BackendRefs[0].DeepCopy()
	other.Weight = ptr.To(int32(20))
	unsupported.Spec.Rules[0].BackendRefs = append(unsupported.Spec.Rules[0].BackendRefs, other)
	f.create(t, unsupported, f.candidate("z-candidate"))

	routes, err := f.reconciler.liveGraphRoutes(t.Context(), controller.GatewayIdentity(testConfig(), f.gateway))
	if err != nil {
		t.Fatal(err)
	}
	models, err := collectRouteModelInputs(t.Context(), f.reader, f.gateway, routes)
	if err != nil {
		t.Fatal(err)
	}
	if len(models.logical.Routes) != 3 || len(models.logical.Routes[0].Rules) != 1 {
		t.Fatalf("complete logical inventory lost routes or rules: %#v", models.logical.Routes)
	}
	groups := models.logical.Routes[0].Rules[0].Backends
	if len(groups) != 2 || groups[0].Weight != 80 || groups[1].Weight != 20 {
		t.Fatalf("complete logical inventory lost backend groups: %#v", models.logical.Routes)
	}
	clear(f.reader.counts)
	input := f.observe(t)
	if len(input.Nodes) != 1 || input.Nodes[0].Disposition != cloud.GraphEnsure || input.Nodes[0].Spec.Identity.RouteUID != string(f.route.UID) {
		t.Fatalf("unsupported candidate displaced the selected route: %#v", input.Nodes)
	}
	if !maps.Equal(f.reader.counts, map[string]int{"Service": 1, "EndpointSlice": 1}) {
		t.Fatalf("backend reads during observation = %v", f.reader.counts)
	}
	if err := input.Validate(t.Context()); err != nil {
		t.Fatalf("unchanged graph failed validation: %v", err)
	}
	if !maps.Equal(f.reader.counts, map[string]int{"Service": 2, "EndpointSlice": 2}) {
		t.Fatalf("backend reads after the mutation fence = %v", f.reader.counts)
	}
}

func TestRouteGraphFenceDetectsBackendDependencyChanges(t *testing.T) {
	for _, change := range []string{
		"Service replacement", "EndpointSlice addition", "EndpointSlice deletion", "EndpointSlice replacement",
		"EndpointSlice label move", "missing Service creation", "ReferenceGrant revocation",
	} {
		t.Run(change, func(t *testing.T) {
			f := newRouteModelInputFixture(t)
			var grant *gatewayv1.ReferenceGrant
			switch change {
			case "missing Service creation":
				candidate := f.candidate("z-missing")
				candidate.Spec.Rules[0].BackendRefs[0].Name = "missing"
				f.create(t, candidate)
			case "ReferenceGrant revocation":
				candidate := f.candidate("z-cross-namespace")
				candidate.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("shared"))
				service := f.service.DeepCopy()
				service.Namespace, service.ResourceVersion, service.UID = "shared", "", "shared-service"
				grant = &gatewayv1.ReferenceGrant{
					ObjectMeta: metav1.ObjectMeta{Namespace: "shared", Name: "allow"},
					Spec: gatewayv1.ReferenceGrantSpec{
						From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: gatewayv1.Namespace(f.route.Namespace)}},
						To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1.ObjectName(service.Name))}},
					},
				}
				f.create(t, candidate, service, grant)
			}
			input := f.observe(t)
			if len(input.Nodes) != 1 || input.Nodes[0].Disposition != cloud.GraphEnsure {
				t.Fatalf("selected route was not ensured: %#v", input.Nodes)
			}
			switch change {
			case "Service replacement":
				replaceRouteModelInputObject(t, f.client, f.service)
			case "EndpointSlice addition":
				added := f.endpoints.DeepCopy()
				added.Name, added.UID, added.ResourceVersion = "new-slice", "new-slice", ""
				f.create(t, added)
			case "EndpointSlice deletion":
				if err := f.client.Delete(t.Context(), f.endpoints); err != nil {
					t.Fatal(err)
				}
			case "EndpointSlice replacement":
				replaceRouteModelInputObject(t, f.client, f.endpoints)
			case "EndpointSlice label move":
				f.endpoints.Labels[discoveryv1.LabelServiceName] = "other"
				if err := f.client.Update(t.Context(), f.endpoints); err != nil {
					t.Fatal(err)
				}
			case "missing Service creation":
				created := f.service.DeepCopy()
				created.Name, created.UID, created.ResourceVersion = "missing", "new-service", ""
				f.create(t, created)
			case "ReferenceGrant revocation":
				if err := f.client.Delete(t.Context(), grant); err != nil {
					t.Fatal(err)
				}
			}
			if err := input.Validate(t.Context()); !errors.Is(err, errHTTPRouteChanged) {
				t.Fatalf("changed dependency passed the mutation fence: %v", err)
			}
		})
	}
}

func TestRouteModelFenceDetectsNamespaceLabelChanges(t *testing.T) {
	f := newRouteModelInputFixture(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.route.Namespace, Labels: map[string]string{"team": "application"}}}
	f.create(t, namespace)
	gateway := f.gateway.DeepCopy()
	gateway.Spec.Listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
		From: ptr.To(gatewayv1.NamespacesFromSelector), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "application"}},
	}}
	inputs, err := collectRouteModelInputs(t.Context(), f.reader, gateway, []gatewayv1.HTTPRoute{*f.route})
	if err != nil {
		t.Fatal(err)
	}
	if err := inputs.validate(t.Context(), f.reader); err != nil {
		t.Fatalf("unchanged Namespace failed validation: %v", err)
	}
	namespace.Labels["team"] = "another"
	if err := f.client.Update(t.Context(), namespace); err != nil {
		t.Fatal(err)
	}
	if err := inputs.validate(t.Context(), f.reader); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("changed Namespace labels passed the mutation fence: %v", err)
	}
}

func TestRouteModelPreservesPendingServiceDeletion(t *testing.T) {
	f := newRouteModelInputFixture(t)
	f.service.Finalizers = []string{"example.test/hold"}
	if err := f.client.Update(t.Context(), f.service); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(t.Context(), f.service); err != nil {
		t.Fatal(err)
	}
	_, err := f.reconciler.buildRouteModelWithReader(t.Context(), f.reader, f.route, f.gateway)
	var buildError *routeBuildError
	if !errors.As(err, &buildError) || buildError.kind != routeErrorPending {
		t.Fatalf("deleting Service must remain pending: %v", err)
	}
	status := statusForRouteBuildError(err)
	if status.resolved != metav1.ConditionTrue || status.programmedReason != string(gatewayv1.RouteReasonPending) {
		t.Fatalf("deleting Service changed reference status: %#v", status)
	}
	if !maps.Equal(f.reader.counts, map[string]int{"Service": 1}) {
		t.Fatalf("deleting Service caused endpoint reads: %v", f.reader.counts)
	}
}

func TestRouteBindingFenceDetectsNewCandidate(t *testing.T) {
	f := newRouteModelInputFixture(t)
	f.reconciler.clearRouteBinding(f.route)
	if err := f.client.Update(t.Context(), f.route); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.reconciler.buildRouteValidationSnapshot(t.Context(), f.route, f.gateway, routeSnapshotForBinding)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reconciler.sealRouteMutationSnapshot(t.Context(), snapshot, routeBindingOptional); err != nil {
		t.Fatalf("unchanged binding snapshot failed validation: %v", err)
	}
	f.create(t, f.candidate("a-earlier"))
	if err := f.reconciler.sealRouteMutationSnapshot(t.Context(), snapshot, routeBindingOptional); !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("new candidate was not observed before binding: %v", err)
	}
}

func TestRouteGraphCleanupDoesNotReadBackendDependencies(t *testing.T) {
	for _, action := range []graph.Action{graph.DeleteRoute, graph.DeleteGateway} {
		t.Run(string(action), func(t *testing.T) {
			f := newRouteModelInputFixture(t)
			f.route.Annotations[testConfig().RouteLifecycleAnnotation()] = controller.RouteDetaching
			if err := f.client.Update(t.Context(), f.route); err != nil {
				t.Fatal(err)
			}
			f.reader.forbid = true
			input, err := f.reconciler.ObserveRouteGraph(t.Context(), graph.Request{
				Identity: controller.RouteIdentity(testConfig(), f.gateway, f.route), Action: action, Route: f.route,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := input.Validate(t.Context()); err != nil {
				t.Fatalf("cleanup required backend dependencies: %v", err)
			}
			if len(f.reader.counts) != 0 {
				t.Fatalf("cleanup read backend dependencies: %v", f.reader.counts)
			}
		})
	}
}

type routeModelInputFixture struct {
	reconciler *Reconciler
	client     client.Client
	reader     *routeModelDependencyReader
	gateway    *gatewayv1.Gateway
	route      *gatewayv1.HTTPRoute
	service    *corev1.Service
	endpoints  *discoveryv1.EndpointSlice
}

func newRouteModelInputFixture(t *testing.T) *routeModelInputFixture {
	t.Helper()
	class, gateway, route, service, endpoints := endpointRouteFixture(t)
	endpoints.UID = "slice-uid"
	r := &Reconciler{Config: testConfig()}
	r.applyRouteBinding(route, gateway)
	kubeClient := indexedFakeClientBuilder(testScheme(t), r.Config).WithObjects(class, gateway, route, service, endpoints).Build()
	for _, object := range []client.Object{gateway, route, service, endpoints} {
		if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
	}
	reader := &routeModelDependencyReader{Reader: kubeClient, counts: map[string]int{}}
	r.Client, r.APIReader = kubeClient, reader
	return &routeModelInputFixture{reconciler: r, client: kubeClient, reader: reader, gateway: gateway, route: route, service: service, endpoints: endpoints}
}

func (f *routeModelInputFixture) candidate(name string) *gatewayv1.HTTPRoute {
	route := f.route.DeepCopy()
	route.Name, route.UID, route.ResourceVersion = name, types.UID(name), ""
	route.Annotations, route.Finalizers = nil, nil
	return route
}

func (f *routeModelInputFixture) create(t *testing.T, objects ...client.Object) {
	t.Helper()
	for _, object := range objects {
		if err := f.client.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *routeModelInputFixture) observe(t *testing.T) graph.RouteInputs {
	t.Helper()
	input, err := f.reconciler.ObserveRouteGraph(t.Context(), graph.Request{
		Identity: controller.RouteIdentity(testConfig(), f.gateway, f.route), Action: graph.Ensure, Route: f.route,
	}, f.gateway)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func replaceRouteModelInputObject(t *testing.T, kubeClient client.Client, object client.Object) {
	t.Helper()
	if err := kubeClient.Delete(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	object.SetResourceVersion("")
	object.SetUID("replacement")
	if err := kubeClient.Create(t.Context(), object); err != nil {
		t.Fatal(err)
	}
}

type routeModelDependencyReader struct {
	client.Reader
	counts map[string]int
	forbid bool
}

func (r *routeModelDependencyReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	var kind string
	switch object.(type) {
	case *corev1.Service:
		kind = "Service"
	case *corev1.Namespace:
		kind = "Namespace"
	case *corev1.Node:
		return errors.New("Endpoint mode must not read Nodes")
	}
	if kind != "" {
		r.counts[kind]++
		if r.forbid {
			return fmt.Errorf("unexpected %s read during cleanup", kind)
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *routeModelDependencyReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	var kind string
	switch list.(type) {
	case *discoveryv1.EndpointSliceList:
		kind = "EndpointSlice"
	case *gatewayv1.ReferenceGrantList:
		kind = "ReferenceGrant"
	case *corev1.NodeList:
		return errors.New("Endpoint mode must not list Nodes")
	}
	if kind != "" {
		r.counts[kind]++
		if r.forbid {
			return fmt.Errorf("unexpected %s list during cleanup", kind)
		}
	}
	return r.Reader.List(ctx, list, options...)
}
