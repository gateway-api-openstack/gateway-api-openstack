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
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestHTTPRouteMaintainsIndependentGatewayBindings(t *testing.T) {
	cfg := testConfig()
	first := programmedTestGateway(cfg)
	second := first.DeepCopy()
	second.Name = "second"
	second.UID = "gateway-b"
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(first.Name)}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{Name: gatewayv1.ObjectName(second.Name)})
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	service, endpoints, node := validNodePortBackend("default", "backend")
	kube := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(route, first, second).WithObjects(class, first, second, route, service, endpoints, node).Build()
	provider := &recordingProvider{}
	r := newTestReconciler(Reconciler{Client: kube, APIReader: kube, Coordinator: &graph.Coordinator{}, Config: cfg}, provider)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}
	current := &gatewayv1.HTTPRoute{}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if err := kube.Get(t.Context(), req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	bindings, err := controller.StoredRouteBindings(cfg, current)
	if err != nil || len(bindings) != 2 || len(current.Status.Parents) != 2 {
		t.Fatalf("bindings=%+v status=%+v error=%v", bindings, current.Status, err)
	}
	for _, parent := range current.Status.Parents {
		condition := meta.FindStatusCondition(parent.Conditions, cfg.Domain()+"/Programmed")
		if condition == nil || condition.Status != metav1.ConditionTrue {
			t.Fatalf("parent=%+v", parent)
		}
	}
	version := current.ResourceVersion
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(t.Context(), req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != version {
		t.Fatal("converged reconciliation rewrote Route status or bindings")
	}

	current.Spec.ParentRefs = current.Spec.ParentRefs[1:]
	current.Generation++
	if err := kube.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if err := kube.Get(t.Context(), req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	bindings, err = controller.StoredRouteBindings(cfg, current)
	if err != nil || len(bindings) != 1 || bindings[0].Identity.GatewayUID != string(second.UID) || len(current.Status.Parents) != 1 {
		t.Fatalf("remaining bindings=%+v status=%+v error=%v", bindings, current.Status, err)
	}
	if len(provider.DeletedRoutes()) != 1 || provider.DeletedRoutes()[0].GatewayUID != string(first.UID) {
		t.Fatalf("deleted routes=%+v", provider.DeletedRoutes())
	}
	// A restarted reconciler reconstructs the surviving graph from the list.
	restarted := newTestReconciler(Reconciler{Client: kube, APIReader: kube, Coordinator: &graph.Coordinator{}, Config: cfg}, provider)
	if _, err := restarted.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
}

func TestBindRoutePreservesBindingAddedDuringConflict(t *testing.T) {
	cfg := testConfig()
	gateway := programmedTestGateway(cfg)
	other := gateway.DeepCopy()
	other.Name = "other"
	other.UID = "other-gateway"
	route := mapperTestHTTPRoute("default", "api", gatewayv1.ParentReference{Name: "edge"}, "backend")
	route.UID = "route-uid"
	route.Generation = 1
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	service, endpoints, node := validNodePortBackend("default", "backend")
	kube := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(route, gateway).WithObjects(class, gateway, other, route, service, endpoints, node).Build()
	conflicting := &bindingInsertConflictClient{Client: kube, config: cfg, other: other}
	r := newTestReconciler(Reconciler{Client: conflicting, APIReader: kube, Coordinator: &graph.Coordinator{}, Config: cfg}, &recordingProvider{})
	if changed, err := r.bindRoute(t.Context(), route, gateway); err != nil || !changed {
		t.Fatalf("changed=%t error=%v", changed, err)
	}
	var current gatewayv1.HTTPRoute
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(route), &current); err != nil {
		t.Fatal(err)
	}
	bindings, err := controller.StoredRouteBindings(cfg, &current)
	if err != nil || len(bindings) != 2 || !slices.Contains(current.Finalizers, "foreign.example/finalizer") || current.Annotations["foreign.example/value"] != "retained" {
		t.Fatalf("bindings=%+v metadata=%+v error=%v", bindings, current.ObjectMeta, err)
	}
}

type bindingInsertConflictClient struct {
	client.Client
	config     controller.Config
	other      *gatewayv1.Gateway
	conflicted bool
}

func (c *bindingInsertConflictClient) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
	route, ok := object.(*gatewayv1.HTTPRoute)
	if !ok || c.conflicted {
		return c.Client.Patch(ctx, object, patch, opts...)
	}
	c.conflicted = true
	current := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(route), current); err != nil {
		return err
	}
	if err := controller.SetRouteBinding(c.config, current, controller.RouteBinding{Identity: controller.RouteIdentity(c.config, c.other, current), Lifecycle: controller.RouteAttached}); err != nil {
		return err
	}
	current.Finalizers = append(current.Finalizers, "foreign.example/finalizer")
	current.Annotations["foreign.example/value"] = "retained"
	if err := c.Update(ctx, current); err != nil {
		return err
	}
	return apierrors.NewConflict(schema.GroupResource{Group: gatewayv1.GroupName, Resource: "httproutes"}, route.Name, errors.New("concurrent Gateway binding"))
}

func TestLiveGraphInventoryIncludesEveryNamespaceAndDiscardsPartialReads(t *testing.T) {
	cfg := testConfig()
	gateway := programmedTestGateway(cfg)
	bound := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "z-routes", Name: "detached", UID: "bound-route"}}
	if err := (&Reconciler{Config: cfg}).applyRouteBinding(bound, gateway); err != nil {
		t.Fatal(err)
	}
	namespace := gatewayv1.Namespace(gateway.Namespace)
	candidate := mapperTestHTTPRoute("a-routes", "candidate", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name), Namespace: &namespace}, "backend")
	candidate.UID = "candidate-route"
	reader := &pagedRouteInventoryReader{pages: map[string]gatewayv1.HTTPRouteList{"": {ListMeta: metav1.ListMeta{Continue: "next"}, Items: []gatewayv1.HTTPRoute{*bound}}, "next": {Items: []gatewayv1.HTTPRoute{*candidate}}}}
	r := &Reconciler{APIReader: reader, Config: cfg}
	routes, err := r.liveGraphRoutes(t.Context(), controller.GatewayIdentity(cfg, gateway))
	if err != nil || len(routes) != 2 || routes[0].Namespace != "a-routes" {
		t.Fatalf("routes=%v error=%v", routes, err)
	}
	reader.fail = true
	if routes, err := r.liveGraphRoutes(t.Context(), controller.GatewayIdentity(cfg, gateway)); err == nil || routes != nil {
		t.Fatalf("partial routes=%v error=%v", routes, err)
	}
}

type pagedRouteInventoryReader struct {
	client.Reader
	pages map[string]gatewayv1.HTTPRouteList
	fail  bool
}

func (r *pagedRouteInventoryReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	options := &client.ListOptions{}
	for _, opt := range opts {
		opt.ApplyToList(options)
	}
	if options.Namespace != "" {
		return fmt.Errorf("unexpected namespace filter %s", options.Namespace)
	}
	if r.fail && options.Continue == "next" {
		return errors.New("second page failed")
	}
	page := r.pages[options.Continue]
	*(list.(*gatewayv1.HTTPRouteList)) = *page.DeepCopy()
	return nil
}

func TestRuntimeRuleGatePrecedesBindingAndCloudMutation(t *testing.T) {
	f := newRouteModelInputFixture(t)
	f.route.Spec.Rules = append(f.route.Spec.Rules, *f.route.Spec.Rules[0].DeepCopy())
	f.route.Generation++
	if err := f.client.Update(t.Context(), f.route); err != nil {
		t.Fatal(err)
	}
	_, err := f.reconciler.buildRouteValidationSnapshot(t.Context(), f.route, f.gateway, routeSnapshotForEnsure)
	var unsupported *routeBuildError
	if !errors.As(err, &unsupported) || unsupported.kind != routeErrorUnsupported {
		t.Fatalf("error=%v", err)
	}
}

func TestHTTPRouteRecreatedGatewayKeepsCurrentStatusWhenOldCleanupFails(t *testing.T) {
	for _, oldUID := range []string{"a-old-gateway", "z-old-gateway"} {
		t.Run(oldUID, func(t *testing.T) {
			cfg := testConfig()
			gateway := programmedTestGateway(cfg)
			gateway.UID = "m-current-gateway"
			oldGateway := gateway.DeepCopy()
			oldGateway.UID = types.UID(oldUID)
			route := mapperTestHTTPRoute(gateway.Namespace, "api", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, "backend")
			route.UID = "route-uid"
			route.Generation = 1
			if err := (&Reconciler{Config: cfg}).applyRouteBinding(route, oldGateway); err != nil {
				t.Fatal(err)
			}
			class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack", Generation: 1}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
			service, endpoints, node := validNodePortBackend(gateway.Namespace, "backend")
			kube := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(route, gateway).WithObjects(class, gateway, route, service, endpoints, node).Build()
			provider := &failingOldGatewayProvider{oldUID: oldUID}
			r := newTestReconciler(Reconciler{Client: kube, APIReader: kube, Coordinator: &graph.Coordinator{}, Config: cfg}, provider)
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}
			for i := 0; i < 3; i++ {
				if _, err := r.Reconcile(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}
			var current gatewayv1.HTTPRoute
			if err := kube.Get(t.Context(), req.NamespacedName, &current); err != nil {
				t.Fatal(err)
			}
			if len(current.Status.Parents) != 1 {
				t.Fatalf("parent status=%+v", current.Status.Parents)
			}
			programmed := meta.FindStatusCondition(current.Status.Parents[0].Conditions, cfg.Domain()+"/Programmed")
			if programmed == nil || programmed.Status != metav1.ConditionTrue {
				t.Fatalf("replacement Gateway status=%+v", programmed)
			}
			old, found, err := controller.StoredRouteBinding(cfg, &current, oldUID)
			if err != nil || !found || old.CleanupFailure != controller.ProviderReasonOwnershipConflict {
				t.Fatalf("old binding=%+v found=%t error=%v", old, found, err)
			}
			replacement, found, err := controller.StoredRouteBinding(cfg, &current, string(gateway.UID))
			if err != nil || !found || replacement.CleanupFailure != "" || len(provider.RouteSpecs()) == 0 {
				t.Fatalf("current binding=%+v found=%t forwarding calls=%d error=%v", replacement, found, len(provider.RouteSpecs()), err)
			}
		})
	}
}

type failingOldGatewayProvider struct {
	recordingProvider
	oldUID string
}

func (p *failingOldGatewayProvider) ReconcileGatewayGraph(ctx context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	result, err := p.recordingProvider.ReconcileGatewayGraph(ctx, spec)
	if err != nil {
		return result, err
	}
	for i, route := range spec.Routes {
		if route.Disposition == cloud.GraphDelete && route.Spec.Identity.GatewayUID == p.oldUID {
			result.Routes[i].Result = cloud.RouteResult{}
			return result, &cloud.GraphError{Identity: route.Spec.Identity, Err: cloud.ErrOwnershipConflict}
		}
	}
	return result, nil
}
