//go:build e2e

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

package e2e

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
)

func TestRoutingUpdatesRejectReplacedObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		uid    types.UID
		marker string
	}{
		{"replacement", "other-uid", "run-1234"}, {"foreign run", "expected-uid", "another-run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := metav1.ObjectMeta{Namespace: "e2e", UID: test.uid, Annotations: map[string]string{runAnnotation: test.marker}}
			gateway := &gatewayv1.Gateway{ObjectMeta: *metadata.DeepCopy()}
			gateway.Name = gatewayName
			route := &gatewayv1.HTTPRoute{ObjectMeta: *metadata.DeepCopy()}
			route.Name = routeName
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway, route).Build()
			suite := &openStackSuite{client: kube, config: e2eConfig{Namespace: "e2e", RunID: "run-1234"}, gatewayUID: "expected-uid"}
			if err := suite.patchRoutingGateway(context.Background(), gatewayv1.GatewaySpec{}); err == nil {
				t.Fatal("updated unowned Gateway")
			}
			if err := suite.patchRoutingRoute(context.Background(), client.ObjectKeyFromObject(route), "expected-uid", gatewayv1.HTTPRouteSpec{}); err == nil {
				t.Fatal("updated unowned HTTPRoute")
			}
		})
	}
}

func TestEndpointFixtureUsesServiceInputsWithoutNodePorts(t *testing.T) {
	suite := &openStackSuite{config: e2eConfig{Namespace: "e2e", RunID: "run-1234", BackendMemberMode: configv1alpha1.MemberModeEndpoint}}
	service := suite.backendService()
	if service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.ExternalTrafficPolicy != "" || service.Spec.Ports[0].NodePort != 0 {
		t.Fatal("Endpoint fixture depends on NodePort")
	}
	endpoints := []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.10"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}
	manual := endpointSliceForService(service, endpoints, suite.config.RunID)
	if len(manual.OwnerReferences) != 0 || manual.Labels[discoveryv1.LabelServiceName] != service.Name || manual.Ports[0].Port == nil || *manual.Ports[0].Port != backendPort {
		t.Fatal("manual EndpointSlice mapping is invalid")
	}
	manual.Endpoints[0].Addresses[0] = "10.0.0.20"
	*manual.Endpoints[0].Conditions.Ready = false
	if endpoints[0].Addresses[0] != "10.0.0.10" || !*endpoints[0].Conditions.Ready {
		t.Fatal("manual EndpointSlice aliases observed inputs")
	}
}

func TestEndpointCollectionRejectsForeignPodAndAddress(t *testing.T) {
	for _, test := range []struct {
		name      string
		alter     func(*corev1.Pod, *appsv1.ReplicaSet, *discoveryv1.EndpointSlice)
		wantError bool
	}{
		{name: "owned IPv4 endpoint"},
		{name: "foreign ReplicaSet", alter: func(_ *corev1.Pod, rs *appsv1.ReplicaSet, _ *discoveryv1.EndpointSlice) {
			rs.OwnerReferences[0].UID = "foreign"
		}, wantError: true},
		{name: "stale Pod", alter: func(p *corev1.Pod, _ *appsv1.ReplicaSet, _ *discoveryv1.EndpointSlice) { p.UID = "new-pod" }, wantError: true},
		{name: "unrelated address", alter: func(_ *corev1.Pod, _ *appsv1.ReplicaSet, e *discoveryv1.EndpointSlice) {
			e.Endpoints[0].Addresses[0] = "10.0.0.99"
		}, wantError: true},
		{name: "duplicate address", alter: func(_ *corev1.Pod, _ *appsv1.ReplicaSet, e *discoveryv1.EndpointSlice) {
			e.Endpoints = append(e.Endpoints, e.Endpoints[0])
		}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, discoveryv1.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}
			suite := &openStackSuite{config: e2eConfig{Namespace: "e2e", RunID: "run-1234", BackendReplicas: 1}, backendServiceUID: "service-uid", backendDeploymentUID: "deployment-uid"}
			service := suite.backendService()
			service.UID = suite.backendServiceUID
			rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "e2e", Name: "replicas", UID: "replicas-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", UID: suite.backendDeploymentUID, Controller: ptr.To(true)}}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "e2e", Name: "backend-pod", UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: ptr.To(true)}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.10", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			endpoints := endpointSliceForService(service, []discoveryv1.Endpoint{{Addresses: []string{pod.Status.PodIP}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID}}}, suite.config.RunID)
			endpoints.OwnerReferences = []metav1.OwnerReference{{Kind: "Service", UID: service.UID, Controller: ptr.To(true)}}
			if test.alter != nil {
				test.alter(pod, rs, endpoints)
			}
			suite.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(service, rs, pod, endpoints).Build()
			result, err := suite.ownedBackendEndpoints(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("ownedBackendEndpoints returned %d endpoints, %v", len(result), err)
			}
		})
	}
}

func TestExpandedChecksRequireSelectedResults(t *testing.T) {
	config := reportTestConfig(projectModeShared)
	config.HTTPRouting, config.EndpointBackends = true, true
	report := newE2EReport(time.Now(), config)
	for i := range report.Checks {
		report.Checks[i].Status = statusPassed
	}
	for _, name := range []string{checkHTTPRouting, checkEndpointBackends} {
		if err := report.setCheck(name, statusNotRun, checkSummaryFailed); err != nil {
			t.Fatal(err)
		}
		if overallStatus(report.Checks, report.requiredChecks...) != statusNotRun {
			t.Fatal("unexecuted expanded scenario reported success")
		}
		if err := report.setCheck(name, statusPassed, checkSummaryPassed); err != nil {
			t.Fatal(err)
		}
	}
}
