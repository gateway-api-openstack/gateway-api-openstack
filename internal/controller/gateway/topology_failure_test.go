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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestGatewayBindingHandlesTopologyProviderFailures(t *testing.T) {
	cause := errors.New("token=private-token response=private-body")
	for _, test := range []struct {
		name        string
		providerErr error
		wantError   bool
	}{
		{name: "unclassified", providerErr: cause, wantError: true},
		{name: "authorization", providerErr: cloud.NewProviderError(cloud.ErrorCategoryAuthorization, cause)},
		{name: "timeout", providerErr: cloud.NewProviderError(cloud.ErrorCategoryTimeout, cause), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge", UID: "gateway-uid", Generation: 3},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "openstack", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
				Status:     gatewayv1.GatewayStatus{Conditions: []metav1.Condition{{Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionFalse, Reason: "Pending", Message: "previous", ObservedGeneration: 2, LastTransitionTime: metav1.Now()}}},
			}
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
			provider := &recordingProvider{}
			recorder := events.NewFakeRecorder(2)
			reconciler := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Config: cfg, Coordinator: &graph.Coordinator{}, Recorder: recorder, TopologyResolver: failingTopologyResolver{err: test.providerErr}}, provider)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}
			result, err := reconciler.Reconcile(t.Context(), request)
			if test.wantError {
				if !errors.Is(err, cause) || strings.Contains(err.Error(), "private-") {
					t.Fatalf("Reconcile() error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if errors.Is(test.providerErr, cloud.ErrAuthorization) {
				assertJitteredRequeue(t, result.RequeueAfter, providerFailureRequeue)
				if event := receiveEvent(t, recorder); strings.Contains(event, "private-") || !strings.Contains(event, "AuthorizationFailed") {
					t.Fatalf("Event = %q", event)
				}
				if _, err := reconciler.Reconcile(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				select {
				case event := <-recorder.Events:
					t.Fatalf("duplicate Event = %q", event)
				default:
				}
			} else if result != (ctrl.Result{}) {
				t.Fatalf("result = %#v", result)
			}
			current := &gatewayv1.Gateway{}
			if err := kubeClient.Get(t.Context(), request.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			if len(current.Finalizers) != 0 || controller.HasGatewayTopologyBinding(cfg, current) || len(provider.gatewaySpecs) != 0 {
				t.Fatal("failed topology resolution acquired a binding or mutated the cloud")
			}
			programmed := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
			if programmed == nil || programmed.ObservedGeneration != 2 || strings.Contains(programmed.Message, "private-") {
				t.Fatalf("Programmed = %#v", programmed)
			}
			if policy, classified := controller.ProviderFailurePolicyFor(test.providerErr); classified && programmed.Message != policy.Message {
				t.Fatalf("Programmed = %#v", programmed)
			}
		})
	}
}

type failingTopologyResolver struct{ err error }

func (r failingTopologyResolver) ResolveTopology(context.Context, cloud.TopologyRequest) (cloud.Topology, error) {
	return cloud.Topology{}, r.err
}

func TestGatewaySpecCopiesFrozenSecurityGroups(t *testing.T) {
	topology := testBoundTopology()
	topology.Topology.VIPSecurityGroupIDs = []string{"33333333-3333-3333-3333-333333333333"}
	r := &Reconciler{Config: testConfig()}
	spec := r.gatewaySpec(&gatewayv1.Gateway{}, &gatewayv1.Listener{Name: "http", Port: 80}, topology)
	if len(spec.VIPSecurityGroupIDs) != 1 || spec.VIPSecurityGroupIDs[0] != topology.Topology.VIPSecurityGroupIDs[0] {
		t.Fatalf("Gateway security groups=%v", spec.VIPSecurityGroupIDs)
	}
	spec.VIPSecurityGroupIDs[0] = "changed"
	if topology.Topology.VIPSecurityGroupIDs[0] != "33333333-3333-3333-3333-333333333333" {
		t.Fatal("Gateway model aliases the frozen security configuration")
	}
}
