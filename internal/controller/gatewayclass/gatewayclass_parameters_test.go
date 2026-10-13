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

package gatewayclass

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func TestGatewayClassRequiresParameters(t *testing.T) {
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "missing", Generation: 2}, Spec: gatewayv1.GatewayClassSpec{ControllerName: testConfig().ControllerName}}
	reconciler, reader := newGatewayClassReconciler(t, class)
	reconcileGatewayClass(t, reconciler, class.Name)
	current := &gatewayv1.GatewayClass{}
	if err := reader.Get(context.Background(), client.ObjectKeyFromObject(class), current); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != string(gatewayv1.GatewayClassReasonInvalidParameters) {
		t.Fatalf("Accepted=%#v", accepted)
	}
}

func TestGatewayClassCloudFailurePreservesEvaluatedGenerationAndRedactsDetails(t *testing.T) {
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "denied", UID: "class-uid", Generation: 3},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: testConfig().ControllerName, ParametersRef: testParametersRef()},
		Status:     gatewayv1.GatewayClassStatus{Conditions: []metav1.Condition{{Type: string(gatewayv1.GatewayClassConditionStatusAccepted), Status: metav1.ConditionTrue, ObservedGeneration: 2, Reason: "Accepted", Message: "previous", LastTransitionTime: metav1.Now()}}},
	}
	reconciler, reader := newGatewayClassReconciler(t, class)
	reconciler.TopologyResolver = testTopologyResolver{err: cloud.NewProviderError(cloud.ErrorCategoryAuthorization, errors.New("private-project-id token=private-token"))}
	recorder := events.NewFakeRecorder(2)
	reconciler.Recorder = recorder
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: class.Name}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	current := &gatewayv1.GatewayClass{}
	if err := reader.Get(context.Background(), client.ObjectKeyFromObject(class), current); err != nil {
		t.Fatal(err)
	}
	accepted := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionUnknown || accepted.ObservedGeneration != 2 || accepted.Reason != "AuthorizationFailed" || strings.Contains(accepted.Message, "private-") {
		t.Fatalf("Accepted=%#v", accepted)
	}
	select {
	case event := <-recorder.Events:
		if strings.Contains(event, "private-") {
			t.Fatalf("event leaked details: %s", event)
		}
	default:
		t.Fatal("expected warning Event")
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("duplicate warning Event: %s", event)
	default:
	}
}

func TestGatewayClassParameterWatchUsesExactManagedReferences(t *testing.T) {
	matching := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "matching"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: testConfig().ControllerName, ParametersRef: testParametersRef()}}
	otherController := matching.DeepCopy()
	otherController.Name = "other-controller"
	otherController.Spec.ControllerName = "other.example/controller"
	otherParameters := matching.DeepCopy()
	otherParameters.Name = "other-parameters"
	otherParameters.Spec.ParametersRef.Name = "different"
	wrongGroup := matching.DeepCopy()
	wrongGroup.Name = "wrong-group"
	wrongGroup.Spec.ParametersRef.Group = "other.example"
	reconciler, _ := newGatewayClassReconciler(t, matching, otherController, otherParameters, wrongGroup)
	requests := reconciler.gatewayClassRequestsForParameters(context.Background(), testClassParameters())
	if len(requests) != 1 || requests[0].Name != "matching" {
		t.Fatalf("requests=%#v", requests)
	}
}

func TestGatewayClassReturnsRedactedTopologyError(t *testing.T) {
	cause := errors.New("token=private-token response=private-body")
	for _, providerErr := range []error{cause, cloud.NewProviderError(cloud.ErrorCategoryTimeout, cause)} {
		class := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "failed", UID: "class-uid", Generation: 3},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: testConfig().ControllerName, ParametersRef: testParametersRef()},
			Status:     gatewayv1.GatewayClassStatus{Conditions: []metav1.Condition{{Type: string(gatewayv1.GatewayClassConditionStatusAccepted), Status: metav1.ConditionTrue, ObservedGeneration: 2, Reason: "Accepted", Message: "previous", LastTransitionTime: metav1.Now()}}},
		}
		reconciler, reader := newGatewayClassReconciler(t, class)
		reconciler.TopologyResolver = testTopologyResolver{err: providerErr}
		result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(class)})
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "private-") || result != (ctrl.Result{}) {
			t.Fatalf("result=%#v error=%v", result, err)
		}
		current := &gatewayv1.GatewayClass{}
		if err := reader.Get(t.Context(), client.ObjectKeyFromObject(class), current); err != nil {
			t.Fatal(err)
		}
		accepted := meta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
		if accepted == nil || accepted.ObservedGeneration != 2 || strings.Contains(accepted.Message, "private-") {
			t.Fatalf("Accepted=%#v", accepted)
		}
	}
}
