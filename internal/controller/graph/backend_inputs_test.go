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

package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/model"
)

func TestCollectBackendInputsSharesReadsAcrossRoutesAndRules(t *testing.T) {
	gateway := backendInputGateway()
	first := backendInputRoute("first", "app", backendInputReference("stable", "", 80, 80), backendInputReference("canary", "", 80, 20))
	first.Spec.Rules = append(first.Spec.Rules, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{
		backendInputReference("stable", "", 8080, 0), backendInputReference("missing", "", 80, 3), backendInputReference("empty", "", 80, 2),
	}})
	second := backendInputRoute("second", "app", backendInputReference("stable", "", 80, 7))
	stable, canary, empty := backendInputService("stable", "app"), backendInputService("canary", "app"), backendInputService("empty", "app")
	stable.Spec.Ports = append(stable.Spec.Ports, corev1.ServicePort{Name: "alternate", Port: 8080})
	stableEndpoints := backendInputEndpoints(stable, "192.0.2.10", "192.0.2.11")
	stableEndpoints.Ports = append(stableEndpoints.Ports, discoveryv1.EndpointPort{Name: ptr.To("alternate"), Port: ptr.To(int32(9000))})
	canaryEndpoints := backendInputEndpoints(canary, "192.0.2.10")
	reader := newBackendInputReader(t, stable, canary, empty, stableEndpoints, canaryEndpoints)
	routes := []gatewayv1.HTTPRoute{second, first}
	input, err := CollectBackendInputs(context.Background(), reader, gateway, routes)
	if err != nil {
		t.Fatal(err)
	}
	wantReads := []string{
		"Service app/canary", "EndpointSlices app/canary", "Service app/empty", "EndpointSlices app/empty",
		"Service app/missing", "Service app/stable", "EndpointSlices app/stable",
	}
	if !slices.Equal(reader.reads, wantReads) {
		t.Fatalf("reads = %v, want %v", reader.reads, wantReads)
	}
	compiled, err := model.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Routes) != 2 || len(compiled.Routes[0].Rules) != 2 {
		t.Fatalf("route inventory was not preserved: %#v", compiled.Routes)
	}
	groups := compiled.Routes[0].Rules[0].Backends
	if groups[0].Weight != 80 || groups[1].Weight != 20 || len(groups[0].Endpoints) != 2 || len(groups[1].Endpoints) != 1 {
		t.Fatalf("Service groups lost their shares or overlapping endpoints: %#v", groups)
	}
	otherGroups := compiled.Routes[0].Rules[1].Backends
	if otherGroups[0].Weight != 0 || otherGroups[0].Endpoints[0].Port != 9000 {
		t.Fatalf("zero-weight alternate port group = %#v", otherGroups[0])
	}
	if otherGroups[1].State != model.BackendInvalid || otherGroups[1].Weight != 3 || otherGroups[2].State != model.BackendResolved || len(otherGroups[2].Endpoints) != 0 {
		t.Fatalf("missing and empty backends were conflated: %#v", otherGroups)
	}
	if compiled.Routes[1].Rules[0].Backends[0].Weight != 7 {
		t.Fatal("shared Service reads merged independent Route groups")
	}
	// Copies belong to this observation, even when the supplied inputs came from a cache.
	input.Gateway.Spec.Listeners[0].Port = 8081
	input.Routes[0].Spec.Rules[0].BackendRefs[0].Weight = ptr.To(int32(99))
	if gateway.Spec.Listeners[0].Port != 80 || *first.Spec.Rules[0].BackendRefs[0].Weight != 80 || routes[0].Name != "second" {
		t.Fatal("collecting or changing an input mutated the caller's objects")
	}

	reader.reads = nil
	forward, err := CollectBackendInputs(context.Background(), reader, gateway, []gatewayv1.HTTPRoute{first, second})
	if err != nil {
		t.Fatal(err)
	}
	reader.reads = nil
	reverse, err := CollectBackendInputs(context.Background(), reader, gateway, []gatewayv1.HTTPRoute{second, first})
	if err != nil || !reflect.DeepEqual(forward, reverse) || !slices.Equal(reader.reads, wantReads) {
		t.Fatalf("route ordering changed the observation: err=%v, reads=%v", err, reader.reads)
	}
}

func TestCollectBackendInputsChecksGrantsBeforeReadingTargets(t *testing.T) {
	service := backendInputService("backend", "shared")
	grant := backendInputGrant("shared", "app", "backend")
	route := backendInputRoute("route", "app", backendInputReference("backend", "shared", 80, 4))
	otherRoute := backendInputRoute("other", "other", backendInputReference("backend", "shared", 80, 6))
	reader := newBackendInputReader(t, service, backendInputEndpoints(service, "192.0.2.10"))
	collect := func() model.Input {
		t.Helper()
		reader.reads = nil
		input, err := CollectBackendInputs(context.Background(), reader, backendInputGateway(), []gatewayv1.HTTPRoute{route, otherRoute})
		if err != nil {
			t.Fatal(err)
		}
		return input
	}
	assertDenied := func(input model.Input) {
		t.Helper()
		if !slices.Equal(reader.reads, []string{"ReferenceGrants shared"}) || len(input.Services) != 0 || len(input.EndpointSlices) != 0 {
			t.Fatalf("unauthorized target was read: %v", reader.reads)
		}
		group := model.ResolveBackends(&route, route.Spec.Rules[0], input)[0]
		if group.Authorization != model.BackendAuthorizationDenied || group.Weight != 4 || group.ServiceUID != "" {
			t.Fatalf("denied backend = %#v", group)
		}
	}
	assertDenied(collect())
	if err := reader.client.Create(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	allowed := collect()
	if !slices.Equal(reader.reads, []string{"ReferenceGrants shared", "Service shared/backend", "EndpointSlices shared/backend"}) {
		t.Fatalf("authorized shared reads = %v", reader.reads)
	}
	if group := model.ResolveBackends(&route, route.Spec.Rules[0], allowed)[0]; group.State != model.BackendResolved || group.Weight != 4 {
		t.Fatalf("allowed backend = %#v", group)
	}
	if group := model.ResolveBackends(&otherRoute, otherRoute.Spec.Rules[0], allowed)[0]; group.Authorization != model.BackendAuthorizationDenied || group.ServiceUID != "" {
		t.Fatalf("another Route's grant exposed target observations: %#v", group)
	}
	if err := reader.client.Delete(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	assertDenied(collect())

	grant.ResourceVersion = ""
	grant.Finalizers = []string{"example.test/hold"}
	if err := reader.client.Create(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	if err := reader.client.Delete(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	deleting := collect()
	assertDenied(deleting)
	if len(deleting.ReferenceGrants) != 1 || deleting.ReferenceGrants[0].DeletionTimestamp.IsZero() {
		t.Fatal("test did not retain a deleting ReferenceGrant")
	}
}

func TestCollectBackendInputsObservesServiceReplacement(t *testing.T) {
	service := backendInputService("backend", "app")
	service.UID = "original"
	reader := newBackendInputReader(t, service)
	route := backendInputRoute("route", "app", backendInputReference("backend", "", 80, 1))
	collect := func() model.BackendGroup {
		t.Helper()
		input, err := CollectBackendInputs(context.Background(), reader, backendInputGateway(), []gatewayv1.HTTPRoute{route})
		if err != nil {
			t.Fatal(err)
		}
		return model.ResolveBackends(&route, route.Spec.Rules[0], input)[0]
	}
	if group := collect(); group.ServiceUID != "original" || group.State != model.BackendResolved {
		t.Fatalf("original backend = %#v", group)
	}
	if err := reader.client.Delete(context.Background(), service); err != nil {
		t.Fatal(err)
	}
	if group := collect(); group.State != model.BackendInvalid || group.Diagnostic.Reason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Fatalf("deleted backend = %#v", group)
	}
	replacement := backendInputService("backend", "app")
	replacement.UID = "replacement"
	replacement.Spec.Ports[0].Port = 8080
	if err := reader.client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if group := collect(); group.ServiceUID != "replacement" || group.State != model.BackendInvalid {
		t.Fatalf("replacement port change was not observed: %#v", group)
	}
	route.Spec.Rules[0].BackendRefs[0].Port = ptr.To(gatewayv1.PortNumber(8080))
	if group := collect(); group.ServiceUID != "replacement" || group.State != model.BackendResolved {
		t.Fatalf("updated reference = %#v", group)
	}

	replacement.Finalizers = []string{"example.test/hold"}
	if err := reader.client.Update(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := reader.client.Delete(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	reader.reads = nil
	input, err := CollectBackendInputs(context.Background(), reader, backendInputGateway(), []gatewayv1.HTTPRoute{route})
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Services) != 1 || input.Services[0].UID != replacement.UID || input.Services[0].DeletionTimestamp.IsZero() {
		t.Fatalf("terminating Service observation was lost: %#v", input.Services)
	}
	if len(input.EndpointSlices) != 0 || !slices.Equal(reader.reads, []string{"Service app/backend"}) {
		t.Fatalf("terminating Service caused endpoint reads: %v", reader.reads)
	}
	group := model.ResolveBackends(&route, route.Spec.Rules[0], input)[0]
	if group.State != model.BackendInvalid || group.ServiceUID != "" {
		t.Fatalf("compiler resolved a terminating Service: %#v", group)
	}
}

func TestCollectBackendInputsReadsNamespacesOnlyForSelectors(t *testing.T) {
	gateway := backendInputGateway()
	route := backendInputRoute("route", "tenant", backendInputReference("backend", "", 80, 1))
	route.Spec.ParentRefs[0].Namespace = ptr.To(gatewayv1.Namespace("app"))
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: map[string]string{"team": "app"}}}
	reader := newBackendInputReader(t, namespace)
	collect := func() model.Input {
		t.Helper()
		reader.reads = nil
		input, err := CollectBackendInputs(context.Background(), reader, gateway, []gatewayv1.HTTPRoute{route})
		if err != nil {
			t.Fatal(err)
		}
		return input
	}
	if input := collect(); len(input.Namespaces) != 0 || !slices.Equal(reader.reads, []string{"Service tenant/backend"}) {
		t.Fatalf("default attachment read Namespaces: %v", reader.reads)
	}
	gateway.Spec.Listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
		From: ptr.To(gatewayv1.NamespacesFromSelector), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "app"}},
	}}
	before := collect()
	if len(before.Namespaces) != 1 || !slices.Equal(reader.reads, []string{"Namespace tenant", "Service tenant/backend"}) {
		t.Fatalf("selector namespace reads = %v", reader.reads)
	}
	compiled, err := model.Compile(before)
	if err != nil || compiled.Routes[0].Parents[0].State != model.AttachmentAccepted {
		t.Fatalf("matching namespace attachment = %#v, err=%v", compiled.Routes, err)
	}
	namespace.Labels["team"] = "other"
	if err := reader.client.Update(context.Background(), namespace); err != nil {
		t.Fatal(err)
	}
	compiled, err = model.Compile(collect())
	if err != nil || compiled.Routes[0].Parents[0].State != model.AttachmentRejected {
		t.Fatalf("changed namespace attachment = %#v, err=%v", compiled.Routes, err)
	}
	if before.Namespaces[0].Labels["team"] != "app" {
		t.Fatal("later namespace change modified an earlier observation")
	}
}

func TestCollectBackendInputsFailsWithoutPartialObservation(t *testing.T) {
	service := backendInputService("backend", "shared")
	route := backendInputRoute("route", "app", backendInputReference("backend", "shared", 80, 1))
	gateway := backendInputGateway()
	gateway.Spec.Listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
		From: ptr.To(gatewayv1.NamespacesFromSelector), Selector: &metav1.LabelSelector{},
	}}
	failure := errors.New("read failed")
	for _, operation := range []string{"Namespace app", "ReferenceGrants shared", "Service shared/backend", "EndpointSlices shared/backend"} {
		t.Run(operation, func(t *testing.T) {
			reader := newBackendInputReader(t, service, backendInputGrant("shared", "app", "backend"))
			reader.failures = map[string]error{operation: failure}
			input, err := CollectBackendInputs(context.Background(), reader, gateway, []gatewayv1.HTTPRoute{route})
			if !errors.Is(err, failure) || !reflect.DeepEqual(input, model.Input{}) {
				t.Fatalf("failed observation = %#v, error=%v", input, err)
			}
		})
	}
}

func TestCollectBackendInputsSkipsUnsupportedBackendKinds(t *testing.T) {
	reference := backendInputReference("secret", "other", 80, 1)
	reference.Kind = ptr.To(gatewayv1.Kind("Secret"))
	route := backendInputRoute("route", "app", reference)
	reader := newBackendInputReader(t)
	input, err := CollectBackendInputs(context.Background(), reader, backendInputGateway(), []gatewayv1.HTTPRoute{route})
	if err != nil || len(reader.reads) != 0 {
		t.Fatalf("unsupported backend caused reads: %v, err=%v", reader.reads, err)
	}
	if group := model.ResolveBackends(&route, route.Spec.Rules[0], input)[0]; group.Diagnostic.Reason != string(gatewayv1.RouteReasonInvalidKind) {
		t.Fatalf("unsupported kind = %#v", group)
	}
}

type backendInputReader struct {
	client   client.Client
	reads    []string
	failures map[string]error
}

func newBackendInputReader(t *testing.T, objects ...client.Object) *backendInputReader {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, discoveryv1.AddToScheme, gatewayv1.Install} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return &backendInputReader{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
}

func (r *backendInputReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	var operation string
	switch object.(type) {
	case *corev1.Service:
		operation = "Service " + key.String()
	case *corev1.Namespace:
		operation = "Namespace " + key.Name
	default:
		return fmt.Errorf("unexpected get of %T", object)
	}
	r.reads = append(r.reads, operation)
	if err := r.failures[operation]; err != nil {
		return err
	}
	return r.client.Get(ctx, key, object, options...)
}

func (r *backendInputReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	var opts client.ListOptions
	opts.ApplyOptions(options)
	var operation string
	switch list.(type) {
	case *discoveryv1.EndpointSliceList:
		if opts.Namespace == "" || opts.LabelSelector == nil {
			return errors.New("EndpointSlice list must select a Service and namespace")
		}
		name, ok := opts.LabelSelector.RequiresExactMatch(discoveryv1.LabelServiceName)
		if !ok {
			return errors.New("EndpointSlice list must select an exact Service")
		}
		operation = "EndpointSlices " + opts.Namespace + "/" + name
	case *gatewayv1.ReferenceGrantList:
		if opts.Namespace == "" {
			return errors.New("ReferenceGrant list must select a namespace")
		}
		operation = "ReferenceGrants " + opts.Namespace
	default:
		return fmt.Errorf("unexpected list of %T", list)
	}
	r.reads = append(r.reads, operation)
	if err := r.failures[operation]; err != nil {
		return err
	}
	return r.client.List(ctx, list, options...)
}

func backendInputGateway() *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "app", UID: "gateway"},
		Spec:       gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}}},
	}
}

func backendInputRoute(name, namespace string, references ...gatewayv1.HTTPBackendRef) gatewayv1.HTTPRoute {
	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name)},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}},
			Rules:           []gatewayv1.HTTPRouteRule{{BackendRefs: references}},
		},
	}
}

func backendInputReference(name, namespace string, port, weight int32) gatewayv1.HTTPBackendRef {
	ref := gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(name), Port: ptr.To(gatewayv1.PortNumber(port))},
		Weight:                 ptr.To(weight),
	}}
	if namespace != "" {
		ref.Namespace = ptr.To(gatewayv1.Namespace(namespace))
	}
	return ref
}

func backendInputService(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name)},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{{Name: "http", Port: 80}}},
	}
}

func backendInputEndpoints(service *corev1.Service, addresses ...string) *discoveryv1.EndpointSlice {
	result := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: service.Name + "-slice", Namespace: service.Namespace, Labels: map[string]string{discoveryv1.LabelServiceName: service.Name}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To(int32(8080))}},
	}
	for _, address := range addresses {
		result.Endpoints = append(result.Endpoints, discoveryv1.Endpoint{Addresses: []string{address}})
	}
	return result
}

func backendInputGrant(namespace, source, service string) *gatewayv1.ReferenceGrant {
	return &gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "allow", Namespace: namespace},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: gatewayv1.Namespace(source)}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1.ObjectName(service))}},
		},
	}
}
