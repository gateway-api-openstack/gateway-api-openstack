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

	corev1 "k8s.io/api/core/v1"
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

func TestGatewayGroupsHTTPListenersAndPreservesLogicalStatus(t *testing.T) {
	cfg := testConfig()
	gateway := programmedTestGateway(cfg)
	gateway.Spec.Listeners = []gatewayv1.Listener{
		{Name: "a", Protocol: gatewayv1.HTTPProtocolType, Port: 80, Hostname: ptr.To(gatewayv1.Hostname("a.example.com"))},
		{Name: "b", Protocol: gatewayv1.HTTPProtocolType, Port: 80, Hostname: ptr.To(gatewayv1.Hostname("*.example.com"))},
		{Name: "other-port", Protocol: gatewayv1.HTTPProtocolType, Port: 8080},
	}
	gateway.Status.Listeners = []gatewayv1.ListenerStatus{{Name: "a", Conditions: []metav1.Condition{{Type: "example.net/Custom", Status: metav1.ConditionTrue, Reason: "Preserve", LastTransitionTime: metav1.Now()}}}}
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway).WithObjects(class, gateway).Build()
	provider := &recordingProvider{}
	reconciler := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Coordinator: &graph.Coordinator{}, Config: cfg}, provider)
	if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}); err != nil {
		t.Fatal(err)
	}
	want := []cloud.ListenerSpec{{Key: cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 80}}, {Key: cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: 8080}}}
	if len(provider.Calls) != 1 {
		t.Fatalf("graph requests = %d, want 1", len(provider.Calls))
	}
	call := provider.Calls[0]
	if call.Gateway.Disposition != cloud.GraphEnsure || call.Identity != controller.GatewayIdentity(cfg, gateway) ||
		!reflect.DeepEqual(call.Gateway.Spec.Listeners, want) {
		t.Fatalf("Gateway graph request: %#v", call)
	}
	var current gatewayv1.Gateway
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(gateway), &current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Listeners) != 3 {
		t.Fatalf("logical listener statuses: %#v", current.Status.Listeners)
	}
	for _, listener := range current.Status.Listeners {
		programmed := meta.FindStatusCondition(listener.Conditions, string(gatewayv1.ListenerConditionProgrammed))
		if programmed == nil || programmed.Status != metav1.ConditionTrue {
			t.Fatalf("listener %s not programmed: %#v", listener.Name, listener.Conditions)
		}
	}
	if meta.FindStatusCondition(current.Status.Listeners[0].Conditions, "example.net/Custom") == nil {
		t.Fatal("foreign listener condition lost")
	}
}

func TestGatewayCountsDistinctRoutesUsingCurrentAttachments(t *testing.T) {
	cfg := testConfig()
	gateway := programmedTestGateway(cfg)
	namespaces := gatewayv1.NamespacesFromSelector
	gateway.Spec.Listeners = []gatewayv1.Listener{
		{Name: "api", Protocol: gatewayv1.HTTPProtocolType, Port: 80, Hostname: ptr.To(gatewayv1.Hostname("api.example.com")), AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &namespaces, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"access": "allowed"}}}}},
		{Name: "other", Protocol: gatewayv1.HTTPProtocolType, Port: 80, Hostname: ptr.To(gatewayv1.Hostname("other.example.com"))},
	}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "backend", Name: "route", UID: "route-uid", Generation: 1}, Spec: gatewayv1.HTTPRouteSpec{
		Hostnames: []gatewayv1.Hostname{"api.example.com"},
		CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
			{Name: gatewayv1.ObjectName(gateway.Name), Namespace: ptr.To(gatewayv1.Namespace(gateway.Namespace))},
			{Name: gatewayv1.ObjectName(gateway.Name), Namespace: ptr.To(gatewayv1.Namespace(gateway.Namespace)), SectionName: ptr.To(gatewayv1.SectionName("api"))},
		}},
	}}
	for _, parent := range route.Spec.ParentRefs {
		route.Status.Parents = append(route.Status.Parents, gatewayv1.RouteParentStatus{ParentRef: parent, ControllerName: cfg.ControllerName, Conditions: []metav1.Condition{controller.Condition(string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), "Accepted", route.Generation)}})
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: route.Namespace, Labels: map[string]string{"access": "allowed"}}}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(route).WithObjects(route, namespace).Build()
	reconciler := &Reconciler{Client: kubeClient, Config: cfg}
	counts, err := reconciler.attachedRouteCounts(t.Context(), gateway)
	if err != nil {
		t.Fatal(err)
	}
	if counts["api"] != 1 || counts["other"] != 0 {
		t.Fatalf("counts: %#v", counts)
	}
	namespace.Labels["access"] = "denied"
	if err := kubeClient.Update(t.Context(), namespace); err != nil {
		t.Fatal(err)
	}
	counts, err = reconciler.attachedRouteCounts(t.Context(), gateway)
	if err != nil {
		t.Fatal(err)
	}
	if counts["api"] != 0 || counts["other"] != 0 {
		t.Fatalf("revoked namespace still counted: %#v", counts)
	}
}

func TestGatewayGraphRejectsSpecChangeBeforeStatusCheckpoint(t *testing.T) {
	cfg := testConfig()
	gateway := programmedTestGateway(cfg)
	gateway.Spec.Listeners[0].Port = 8080
	gateway.Generation++
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
	kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway).WithObjects(gateway, class).Build()
	provider := &recordingProvider{}
	reconciler := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Coordinator: &graph.Coordinator{}, Config: cfg}, provider)
	_, err := reconciler.ensureGateway(context.Background(), gateway)
	if !errors.Is(err, controller.ErrGraphInputChanged) || len(provider.GatewaySpecs()) != 0 {
		t.Fatalf("uncheckpointed graph: calls=%d error=%v", len(provider.GatewaySpecs()), err)
	}
}

func TestGatewayFailurePreservesListenerDiagnostics(t *testing.T) {
	tests := []struct {
		name           string
		listeners      []gatewayv1.Listener
		condition      gatewayv1.ListenerConditionType
		reason         gatewayv1.ListenerConditionReason
		status         metav1.ConditionStatus
		supportedKinds bool
	}{
		{
			name: "hostname conflict",
			listeners: []gatewayv1.Listener{
				{Name: "first", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
				{Name: "second", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
			},
			condition:      gatewayv1.ListenerConditionConflicted,
			reason:         gatewayv1.ListenerReasonHostnameConflict,
			status:         metav1.ConditionTrue,
			supportedKinds: true,
		},
		{
			name: "unsupported protocol",
			listeners: []gatewayv1.Listener{{
				Name: "first", Protocol: gatewayv1.UDPProtocolType, Port: 80,
			}},
			condition: gatewayv1.ListenerConditionAccepted,
			reason:    gatewayv1.ListenerReasonUnsupportedProtocol,
			status:    metav1.ConditionFalse,
		},
		{
			name: "unsupported route kind",
			listeners: []gatewayv1.Listener{{
				Name: "first", Protocol: gatewayv1.HTTPProtocolType, Port: 80,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{{Kind: "TCPRoute"}}},
			}},
			condition: gatewayv1.ListenerConditionResolvedRefs,
			reason:    gatewayv1.ListenerReasonInvalidRouteKinds,
			status:    metav1.ConditionFalse,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := programmedTestGateway(testConfig())
			gateway.Spec.Listeners = append(test.listeners, gatewayv1.Listener{Name: "unaffected", Protocol: gatewayv1.HTTPProtocolType, Port: 8080})
			gateway.Status.Listeners = []gatewayv1.ListenerStatus{{Name: "first", Conditions: []metav1.Condition{{
				Type: "example.net/Custom", Status: metav1.ConditionTrue, Reason: "Preserve", LastTransitionTime: metav1.Now(),
			}}}}
			cfg := testConfig()
			class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}}
			kubeClient := indexedFakeClientBuilder(testScheme(t), cfg).WithStatusSubresource(gateway).WithObjects(gateway, class).Build()
			provider := &recordingProvider{}
			reconciler := newTestReconciler(Reconciler{Client: kubeClient, APIReader: kubeClient, Config: cfg, Coordinator: &graph.Coordinator{}}, provider)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)}
			if _, err := reconciler.Reconcile(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if err := kubeClient.Get(t.Context(), request.NamespacedName, gateway); err != nil {
				t.Fatal(err)
			}
			accepted := meta.FindStatusCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
			if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.Reason != string(gatewayv1.GatewayReasonListenersNotValid) {
				t.Fatalf("partial Gateway status: %#v", accepted)
			}
			if len(provider.GatewaySpecs()) != 1 || len(provider.GatewaySpecs()[0].Listeners) != 1 || provider.GatewaySpecs()[0].Listeners[0].Key.Port != 8080 || len(provider.DeletedGateways()) != 0 {
				t.Fatalf("invalid siblings changed valid infrastructure: %#v", provider)
			}
			for _, listener := range gateway.Status.Listeners {
				if listener.Name != "unaffected" && (len(listener.SupportedKinds) != 0) != test.supportedKinds {
					t.Fatalf("rejected listener advertised incorrect route kinds: %#v", listener)
				}
				if listener.Name == "unaffected" {
					for _, conditionType := range []gatewayv1.ListenerConditionType{gatewayv1.ListenerConditionAccepted, gatewayv1.ListenerConditionResolvedRefs} {
						condition := meta.FindStatusCondition(listener.Conditions, string(conditionType))
						if condition == nil || condition.Status != metav1.ConditionTrue {
							t.Fatalf("unrelated listener received a failure: %#v", listener)
						}
					}
					condition := meta.FindStatusCondition(listener.Conditions, string(gatewayv1.ListenerConditionConflicted))
					if condition == nil || condition.Status != metav1.ConditionFalse {
						t.Fatalf("unrelated listener is conflicted: %#v", listener)
					}
				} else {
					condition := meta.FindStatusCondition(listener.Conditions, string(test.condition))
					if condition == nil || condition.Status != test.status || condition.Reason != string(test.reason) {
						t.Fatalf("listener diagnostic lost: %#v", listener)
					}
				}
				programmed := meta.FindStatusCondition(listener.Conditions, string(gatewayv1.ListenerConditionProgrammed))
				want := metav1.ConditionFalse
				if listener.Name == "unaffected" {
					want = metav1.ConditionTrue
				}
				if programmed == nil || programmed.Status != want {
					t.Fatalf("listener programming status: %#v", listener)
				}
			}
			if meta.FindStatusCondition(gateway.Status.Listeners[0].Conditions, "example.net/Custom") == nil {
				t.Fatal("foreign listener condition lost")
			}
			previous := gateway.DeepCopy()
			if _, err := reconciler.Reconcile(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if err := kubeClient.Get(t.Context(), request.NamespacedName, gateway); err != nil {
				t.Fatal(err)
			}
			if gateway.ResourceVersion != previous.ResourceVersion || !reflect.DeepEqual(gateway.Status, previous.Status) {
				t.Fatal("unchanged partial Gateway rewrote status or transition times")
			}
		})
	}
}
