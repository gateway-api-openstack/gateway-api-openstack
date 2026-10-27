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
	"errors"
	"strings"
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestUnsupportedAmphoraActionsDoNotBindOrProvision(t *testing.T) {
	for _, test := range []struct {
		name    string
		filters []gatewayv1.HTTPRouteFilter
		message string
	}{
		{name: "request headers", filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Add:    []gatewayv1.HTTPHeader{{Name: "X-Added", Value: "one"}},
				Set:    []gatewayv1.HTTPHeader{{Name: "X-Replaced", Value: "two"}},
				Remove: []string{"X-Removed"},
			},
		}}, message: "route-scoped header operations"},
		{name: "request hostname redirect", filters: []gatewayv1.HTTPRouteFilter{{
			Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Scheme: ptr.To("https")},
		}}, message: "request-hostname preservation"},
		{name: "static hostname redirect", filters: []gatewayv1.HTTPRouteFilter{{
			Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Hostname: ptr.To(gatewayv1.PreciseHostname("example.test"))},
		}}, message: "fixed hostname is not implemented"},
		{name: "omitted backend", message: "HTTP 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			class, gateway := providerRouteParentObjects(cfg)
			route := mapperTestHTTPRoute(gateway.Namespace, "route", gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}, "backend")
			route.UID, route.Generation = "route-uid", 1
			route.Spec.Rules[0].Filters = test.filters
			route.Spec.Rules[0].BackendRefs = nil
			route.Annotations = map[string]string{"example.test/foreign": "preserve"}
			route.Finalizers = []string{"example.test/foreign"}
			foreignParent := gatewayv1.RouteParentStatus{
				ParentRef: gatewayv1.ParentReference{Name: "other"}, ControllerName: "example.test/foreign",
				Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted", LastTransitionTime: metav1.NewTime(time.Unix(1, 0).UTC())}},
			}
			route.Status.Parents = []gatewayv1.RouteParentStatus{foreignParent}
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(route).WithObjects(class, gateway, route).Build()
			provider := &recordingProvider{}
			r := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Config: cfg, Coordinator: &graph.Coordinator{}}, provider)
			_, err := r.buildRouteSpecWithReader(t.Context(), kubeClient, route, gateway)
			var unsupported *routeBuildError
			if !errors.As(err, &unsupported) || unsupported.kind != routeErrorUnsupported || !strings.Contains(unsupported.message, test.message) {
				t.Fatalf("unsupported action error=%v", err)
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(route)}
			for range 2 {
				if _, err := r.Reconcile(t.Context(), request); err != nil {
					t.Fatal(err)
				}
			}
			var current gatewayv1.HTTPRoute
			if err := kubeClient.Get(t.Context(), request.NamespacedName, &current); err != nil {
				t.Fatal(err)
			}
			if !apiequality.Semantic.DeepEqual(current.Annotations, route.Annotations) || !apiequality.Semantic.DeepEqual(current.Finalizers, route.Finalizers) || len(current.Status.Parents) != 2 {
				t.Fatalf("unsupported route acquired binding metadata or lost foreign status: %#v", current)
			}
			for _, parent := range current.Status.Parents {
				if parent.ControllerName == foreignParent.ControllerName {
					if !apiequality.Semantic.DeepEqual(parent, foreignParent) {
						t.Fatalf("foreign parent status changed: got=%#v want=%#v", parent, foreignParent)
					}
					continue
				}
				accepted := meta.FindStatusCondition(parent.Conditions, string(gatewayv1.RouteConditionAccepted))
				if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != string(gatewayv1.RouteReasonUnsupportedValue) {
					t.Fatalf("unsupported action status=%#v", parent)
				}
			}
			if len(provider.gatewaySpecs)+len(provider.routeSpecs)+len(provider.deletedGateways)+len(provider.deletedRoutes) != 0 {
				t.Fatal("unsupported action called the cloud provider")
			}
		})
	}
}
