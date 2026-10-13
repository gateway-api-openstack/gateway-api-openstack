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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestGatewayCleanupRejectsTopologyWithoutIdentityBinding(t *testing.T) {
	cfg := testConfig()
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid"}}
	if err := controller.BindGatewayTopology(cfg, gateway, testBoundTopology()); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	gateway.DeletionTimestamp = &now
	kubeClient := rawIndexedFakeClientBuilder(testScheme(t), cfg).WithObjects(gateway).Build()
	provider := &recordingProvider{}
	r := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Config: cfg, Coordinator: &graph.Coordinator{}}, provider)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)})
	if err == nil {
		t.Fatal("cleanup accepted topology without its immutable identity binding")
	}
	if len(provider.deletedGateways) != 0 || len(provider.gatewaySpecs) != 0 {
		t.Fatal("incomplete identity authorized a provider call")
	}
	var current gatewayv1.Gateway
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(gateway), &current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gateway.Finalizers, current.Finalizers) || !reflect.DeepEqual(gateway.Annotations, current.Annotations) {
		t.Fatal("cleanup changed incomplete binding metadata")
	}
}

func TestGatewayReplacementRechecksRevisionAfterProviderConfirmsAbsence(t *testing.T) {
	tests := []struct {
		name   string
		change func(*gatewayv1.Gateway)
	}{
		{name: "spec changed", change: func(gateway *gatewayv1.Gateway) {
			gateway.Spec.Listeners[0].Port = 9090
			gateway.Generation++
		}},
		{name: "metadata changed", change: func(gateway *gatewayv1.Gateway) {
			gateway.Annotations["example.net/operator-note"] = "changed during cloud observation"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			gateway := programmedTestGateway(cfg)
			gateway.Spec.Listeners[0].Port = 8080
			gateway.Generation++
			class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
			provider := &gatewayCleanupChangeProvider{afterReady: func(ctx context.Context) error {
				var latest gatewayv1.Gateway
				if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(gateway), &latest); err != nil {
					return err
				}
				test.change(&latest)
				return kubeClient.Update(ctx, &latest)
			}}
			r := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Config: cfg, Coordinator: &graph.Coordinator{}}, provider)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}
			if result, err := r.Reconcile(context.Background(), request); err != nil || result != (ctrl.Result{Requeue: true}) {
				t.Fatalf("replacement status checkpoint = %#v, %v", result, err)
			}
			if len(provider.deletedGateways) != 0 {
				t.Fatal("replacement started before the status checkpoint")
			}
			_, err := r.Reconcile(context.Background(), request)
			if !errors.Is(err, errGatewayChanged) {
				t.Fatalf("replacement after concurrent change = %v, want stale snapshot rejection", err)
			}
			if len(provider.deletedGateways) != 1 || len(provider.gatewaySpecs) != 0 {
				t.Fatalf("provider calls = deletes %d, ensures %d", len(provider.deletedGateways), len(provider.gatewaySpecs))
			}
			var current gatewayv1.Gateway
			if err := kubeClient.Get(context.Background(), request.NamespacedName, &current); err != nil {
				t.Fatal(err)
			}
			if !controllerutil.ContainsFinalizer(&current, cfg.GatewayFinalizer()) ||
				!controller.GatewayTopologyMetadataEqual(cfg, gateway, &current) ||
				current.Annotations[cfg.GatewayListenerPortAnnotation()] != "80" {
				t.Fatal("stale replacement released or rewrote the frozen binding")
			}
			want := gateway.DeepCopy()
			test.change(want)
			if !reflect.DeepEqual(current.Spec, want.Spec) || current.Annotations["example.net/operator-note"] != want.Annotations["example.net/operator-note"] {
				t.Fatal("stale replacement overwrote the concurrent input change")
			}
		})
	}
}

type gatewayCleanupChangeProvider struct {
	recordingProvider
	afterReady func(context.Context) error
}

func (p *gatewayCleanupChangeProvider) ReconcileGatewayGraph(ctx context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	result, err := p.recordingProvider.ReconcileGatewayGraph(ctx, spec)
	if err == nil && spec.Gateway.Disposition == cloud.GraphDelete && result.Outcome.State == cloud.OutcomeReady {
		err = p.afterReady(ctx)
	}
	return result, err
}
