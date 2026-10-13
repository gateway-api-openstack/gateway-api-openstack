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
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestGatewayRejectsMissingComponentOutcome(t *testing.T) {
	cfg := testConfig()
	gateway := programmedTestGateway(cfg)
	gateway.Status = gatewayv1.GatewayStatus{}
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	reconciler := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Config: cfg, Writer: fixedGatewayGraph{result: cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome()}}}, nil)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}); err == nil {
		t.Fatal("missing Gateway component outcome was accepted")
	}
	current := &gatewayv1.Gateway{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.Status, gateway.Status) {
		t.Fatalf("invalid component result changed status: %#v", current.Status)
	}
}

func TestGatewayRouteFailurePreservesStatusAndRedactsDetails(t *testing.T) {
	for _, category := range []cloud.ErrorCategory{cloud.ErrorCategoryRateLimit, cloud.ErrorCategoryAuthorization, ""} {
		t.Run(string(category), func(t *testing.T) {
			cfg := testConfig()
			gateway := programmedTestGateway(cfg)
			class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
			current := &gatewayv1.Gateway{}
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
				t.Fatal(err)
			}
			before := current.DeepCopy()
			cause := errors.New("response=private-body token=secret-token")
			providerErr := cause
			if category != "" {
				providerErr = cloud.NewProviderError(category, cause)
			}
			graphErr := &cloud.GraphError{Identity: cloud.Identity{RouteUID: "route-uid"}, Err: providerErr}
			reconciler := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Config: cfg, Writer: fixedGatewayGraph{err: graphErr}}, nil)
			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)})
			if category == cloud.ErrorCategoryAuthorization {
				if err != nil || result.RequeueAfter <= 0 {
					t.Fatalf("authorization result=%#v error=%v", result, err)
				}
			} else if !errors.Is(err, cause) || strings.Contains(err.Error(), "private-body") || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("route error=%v", err)
			}
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current.Status, before.Status) {
				t.Fatalf("route failure changed Gateway status: before=%#v after=%#v", before.Status, current.Status)
			}
		})
	}
}

type fixedGatewayGraph struct {
	result cloud.GatewayGraphResult
	err    error
}

func (w fixedGatewayGraph) ReconcileGraph(context.Context, graph.Request) (cloud.GatewayGraphResult, error) {
	return w.result, w.err
}
