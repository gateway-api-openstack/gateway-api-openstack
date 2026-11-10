//go:build envtest

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

package controller_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

func testHTTPRouteReferenceIndexUpdates(t *testing.T, apiClient, cacheClient client.Client) {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "reference-index-envtest"}}
	if err := apiClient.Create(t.Context(), namespace); err != nil {
		t.Fatalf("create reference test Namespace: %v", err)
	}
	route := envtestHTTPRoute(namespace.Name, "references", "old-parent", "old-service")
	route.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("old-backends"))
	// References repeated across rules must produce one consumer in each index.
	route.Spec.Rules = append(route.Spec.Rules, *route.Spec.Rules[0].DeepCopy())
	route.Spec.Rules[1].Matches = []gatewayv1.HTTPRouteMatch{{
		Path: &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchPathPrefix), Value: ptr.To("/second")},
	}}
	if err := apiClient.Create(t.Context(), route); err != nil {
		t.Fatalf("create HTTPRoute with repeated backend references: %v", err)
	}
	assertIndex := func(field, value, wantName string) {
		t.Helper()
		waitForEnvtest(t, func(ctx context.Context) (bool, error) {
			var routes gatewayv1.HTTPRouteList
			if err := cacheClient.List(ctx, &routes, client.MatchingFields{field: value}); err != nil {
				return false, err
			}
			if wantName == "" {
				return len(routes.Items) == 0, nil
			}
			return len(routes.Items) == 1 && routes.Items[0].Name == wantName && routes.Items[0].Namespace == namespace.Name, nil
		})
	}
	assertIndex(controller.IndexHTTPRouteByBackendService, "old-backends/old-service", route.Name)
	assertIndex(controller.IndexHTTPRouteByBackendNamespace, "old-backends", route.Name)
	assertIndex(controller.IndexHTTPRouteByParentGateway, namespace.Name+"/old-parent", route.Name)

	base := route.DeepCopy()
	route.Spec.ParentRefs[0].Name = "new-parent"
	for i := range route.Spec.Rules {
		reference := &route.Spec.Rules[i].BackendRefs[0]
		reference.Name = "new-service"
		reference.Namespace = ptr.To(gatewayv1.Namespace("new-backends"))
	}
	if err := apiClient.Patch(t.Context(), route, client.MergeFrom(base)); err != nil {
		t.Fatalf("replace HTTPRoute backend and parent references: %v", err)
	}
	assertIndex(controller.IndexHTTPRouteByBackendService, "old-backends/old-service", "")
	assertIndex(controller.IndexHTTPRouteByBackendNamespace, "old-backends", "")
	assertIndex(controller.IndexHTTPRouteByParentGateway, namespace.Name+"/old-parent", "")
	assertIndex(controller.IndexHTTPRouteByBackendService, "new-backends/new-service", route.Name)
	assertIndex(controller.IndexHTTPRouteByBackendNamespace, "new-backends", route.Name)
	assertIndex(controller.IndexHTTPRouteByParentGateway, namespace.Name+"/new-parent", route.Name)

	base = route.DeepCopy()
	for i := range route.Spec.Rules {
		route.Spec.Rules[i].BackendRefs[0].Namespace = nil
	}
	if err := apiClient.Patch(t.Context(), route, client.MergeFrom(base)); err != nil {
		t.Fatalf("replace HTTPRoute backends with local references: %v", err)
	}
	assertIndex(controller.IndexHTTPRouteByBackendService, "new-backends/new-service", "")
	assertIndex(controller.IndexHTTPRouteByBackendNamespace, "new-backends", "")
	assertIndex(controller.IndexHTTPRouteByBackendNamespace, namespace.Name, "")
	assertIndex(controller.IndexHTTPRouteByBackendService, namespace.Name+"/new-service", route.Name)

	if err := apiClient.Delete(t.Context(), route); err != nil {
		t.Fatalf("delete indexed HTTPRoute: %v", err)
	}
	assertIndex(controller.IndexHTTPRouteByBackendService, namespace.Name+"/new-service", "")
	assertIndex(controller.IndexHTTPRouteByParentGateway, namespace.Name+"/new-parent", "")
}
