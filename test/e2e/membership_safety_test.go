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
	"maps"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/audit"
)

func TestMembershipTransitionPreservesUnchangedResources(t *testing.T) {
	baseline := membershipInventory{members: map[string]struct{}{"first": {}, "second": {}}, parents: [32]byte{1}}
	for _, test := range []struct {
		name     string
		observed membershipInventory
		retained map[string]struct{}
		want     bool
	}{
		{name: "remove one", observed: membershipInventory{members: map[string]struct{}{"first": {}}, parents: baseline.parents}, want: true},
		{name: "replaced survivor", observed: membershipInventory{members: map[string]struct{}{"replacement": {}}, parents: baseline.parents}},
		{name: "recreated pool", observed: membershipInventory{members: map[string]struct{}{"first": {}}, parents: [32]byte{2}}},
		{name: "restore missing member", observed: membershipInventory{members: map[string]struct{}{"first": {}, "new": {}}, parents: baseline.parents}, retained: map[string]struct{}{"first": {}}, want: true},
		{name: "replace both on restore", observed: membershipInventory{members: map[string]struct{}{"new-a": {}, "new-b": {}}, parents: baseline.parents}, retained: map[string]struct{}{"first": {}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := membershipTransitionMatches(baseline, test.observed, test.retained); got != test.want {
				t.Fatalf("membershipTransitionMatches() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestMembershipAuditUsesMemberIDsAndParentGraph(t *testing.T) {
	resources := []audit.ResourceFinding{
		{Service: "Octavia", Type: "LoadBalancer", ID: "load-balancer", Disposition: audit.DispositionMatched},
		{Service: "Octavia", Type: "Listener", ID: "listener", ParentID: "load-balancer", Disposition: audit.DispositionMatched},
		{Service: "Octavia", Type: "Pool", ID: "pool", ParentID: "listener", Disposition: audit.DispositionMatched},
		{Service: "Octavia", Type: "Member", ID: "member", ParentID: "pool", Disposition: audit.DispositionMatched},
	}
	report := audit.Report{Resources: resources, Summary: audit.Summary{KubernetesBindings: 2, OpenStackResources: 4, Matched: 4}}
	inventory, err := membershipFromAudit(report, 1)
	if err != nil || !maps.Equal(inventory.members, map[string]struct{}{"member": {}}) {
		t.Fatalf("membershipFromAudit() = %#v, %v", inventory, err)
	}
	if _, err := membershipFromAudit(report, 2); err == nil {
		t.Fatal("member count mismatch was accepted")
	}
	resources[2].ID = "recreated-pool"
	recreated, err := membershipFromAudit(report, 1)
	if err != nil || recreated.parents == inventory.parents {
		t.Fatal("membership audit did not detect parent replacement")
	}
}

func TestBackendScalingRejectsIdentityAndProfileChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*appsv1.Deployment, *corev1.Service)
	}{
		{name: "deployment replaced", mutate: func(d *appsv1.Deployment, _ *corev1.Service) { d.UID = "replacement" }},
		{name: "deployment marker changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) { d.Annotations[runAnnotation] = "other-run" }},
		{name: "placement changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) { d.Spec.Template.Spec.NodeSelector = nil }},
		{name: "worker separation removed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) { d.Spec.Template.Spec.Affinity = nil }},
		{name: "Node pinned directly", mutate: func(d *appsv1.Deployment, _ *corev1.Service) { d.Spec.Template.Spec.NodeName = "foreign-worker" }},
		{name: "Deployment selector changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) { d.Spec.Selector.MatchLabels["foreign"] = "true" }},
		{name: "Pod labels changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) { d.Spec.Template.Labels["foreign"] = "true" }},
		{name: "additional container", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, corev1.Container{Name: "foreign", Image: "foreign"})
		}},
		{name: "additional init container", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "foreign", Image: "foreign"}}
		}},
		{name: "image changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) { d.Spec.Template.Spec.Containers[0].Image = "different" }},
		{name: "command changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers[0].Command = []string{"foreign"}
		}},
		{name: "arguments changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers[0].Args = []string{"foreign"}
		}},
		{name: "container port changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort++
		}},
		{name: "container protocol changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolUDP
		}},
		{name: "host port added", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers[0].Ports[0].HostPort = 80
		}},
		{name: "readiness path changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path = "/foreign"
		}},
		{name: "readiness period changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers[0].ReadinessProbe.PeriodSeconds++
		}},
		{name: "readiness scheme changed", mutate: func(d *appsv1.Deployment, _ *corev1.Service) {
			d.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
		}},
		{name: "service replaced", mutate: func(_ *appsv1.Deployment, s *corev1.Service) { s.UID = "replacement" }},
		{name: "service marker changed", mutate: func(_ *appsv1.Deployment, s *corev1.Service) { s.Annotations[runAnnotation] = "other-run" }},
		{name: "load balancer service", mutate: func(_ *appsv1.Deployment, s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeLoadBalancer }},
		{name: "cluster policy", mutate: func(_ *appsv1.Deployment, s *corev1.Service) {
			s.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
		}},
		{name: "selector changed", mutate: func(_ *appsv1.Deployment, s *corev1.Service) { s.Spec.Selector["foreign"] = "true" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite, deployment, service := membershipFixture(t)
			test.mutate(deployment, service)
			if err := suite.client.Update(context.Background(), deployment); err != nil {
				t.Fatal(err)
			}
			if err := suite.client.Update(context.Background(), service); err != nil {
				t.Fatal(err)
			}
			if err := suite.scaleBackend(context.Background(), 0); err == nil {
				t.Fatal("changed backend was accepted for scaling")
			}
			var current appsv1.Deployment
			if err := suite.client.Get(context.Background(), client.ObjectKeyFromObject(deployment), &current); err != nil {
				t.Fatal(err)
			}
			if *current.Spec.Replicas != 1 {
				t.Fatal("backend was scaled before its identity and profile were validated")
			}
		})
	}
}

func TestBackendScalingChangesOnlyReplicaCount(t *testing.T) {
	for _, defaulted := range []bool{false, true} {
		suite, deployment, service := membershipFixture(t)
		if defaulted {
			defaultBackendDeployment(deployment)
			if err := suite.client.Update(context.Background(), deployment); err != nil {
				t.Fatal(err)
			}
		}
		if err := suite.scaleBackend(context.Background(), 0); err != nil {
			t.Fatalf("defaulted=%t: scaleBackend() = %v", defaulted, err)
		}
		var current appsv1.Deployment
		if err := suite.client.Get(context.Background(), client.ObjectKeyFromObject(deployment), &current); err != nil {
			t.Fatal(err)
		}
		wanted := deployment.DeepCopy()
		*wanted.Spec.Replicas = 0
		if current.UID != deployment.UID || !equality.Semantic.DeepEqual(current.Spec, wanted.Spec) {
			t.Fatal("scaling changed fields other than the backend replica count")
		}
		var observedService corev1.Service
		if err := suite.client.Get(context.Background(), client.ObjectKeyFromObject(service), &observedService); err != nil {
			t.Fatal(err)
		}
		if observedService.ResourceVersion != service.ResourceVersion {
			t.Fatal("scaling updated the Service")
		}
	}
}

// Defaulting is applied explicitly because the fake client does not run an API
// server. These values reproduce the stored fixture returned by Kubernetes.
func defaultBackendDeployment(deployment *appsv1.Deployment) {
	revisionHistory, progressDeadline := int32(10), int32(600)
	deployment.Spec.RevisionHistoryLimit, deployment.Spec.ProgressDeadlineSeconds = &revisionHistory, &progressDeadline
	percentage := intstr.FromString("25%")
	deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &percentage, MaxSurge: &percentage}}
	pod := &deployment.Spec.Template.Spec
	grace := int64(30)
	pod.TerminationGracePeriodSeconds = &grace
	pod.RestartPolicy, pod.DNSPolicy, pod.SchedulerName = corev1.RestartPolicyAlways, corev1.DNSClusterFirst, corev1.DefaultSchedulerName
	pod.SecurityContext = &corev1.PodSecurityContext{}
	container := &pod.Containers[0]
	container.ImagePullPolicy = corev1.PullIfNotPresent
	container.TerminationMessagePath, container.TerminationMessagePolicy = corev1.TerminationMessagePathDefault, corev1.TerminationMessageReadFile
	container.Ports[0].Protocol = corev1.ProtocolTCP
	container.ReadinessProbe.TimeoutSeconds, container.ReadinessProbe.SuccessThreshold, container.ReadinessProbe.FailureThreshold = 1, 1, 3
	container.ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTP
}

func TestMembershipReadinessRequiresOwnedPodsAndEndpoints(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod, *discoveryv1.EndpointSlice)
		ready  bool
	}{
		{name: "owned ready endpoint", ready: true},
		{name: "foreign Pod", mutate: func(p *corev1.Pod, _ *discoveryv1.EndpointSlice) { p.OwnerReferences[0].UID = "foreign" }},
		{name: "foreign EndpointSlice", mutate: func(_ *corev1.Pod, s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].UID = "foreign" }},
		{name: "stale endpoint Pod", mutate: func(_ *corev1.Pod, s *discoveryv1.EndpointSlice) { s.Endpoints[0].TargetRef.UID = "old-pod" }},
		{name: "wrong endpoint Node", mutate: func(_ *corev1.Pod, s *discoveryv1.EndpointSlice) {
			value := "another-worker"
			s.Endpoints[0].NodeName = &value
		}},
		{name: "duplicate endpoint", mutate: func(_ *corev1.Pod, s *discoveryv1.EndpointSlice) { s.Endpoints = append(s.Endpoints, s.Endpoints[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite, deployment, service := membershipFixture(t)
			pod, endpoints := membershipPodAndEndpoints(suite, service)
			if test.mutate != nil {
				test.mutate(pod, endpoints)
			}
			for _, object := range []client.Object{pod, endpoints} {
				if err := suite.client.Create(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			}
			ready, err := suite.membershipEndpointsReady(context.Background(), deployment, service, 1)
			if test.ready && (err != nil || !ready) || !test.ready && ready {
				t.Fatalf("membershipEndpointsReady() = %t, %v, want ready=%t", ready, err, test.ready)
			}
		})
	}
}

func TestMembershipReadinessWaitsForAllPodsBeforeZeroReplicas(t *testing.T) {
	suite, deployment, service := membershipFixture(t)
	pod, endpoints := membershipPodAndEndpoints(suite, service)
	for _, object := range []client.Object{pod, endpoints} {
		if err := suite.client.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	ready, err := suite.membershipEndpointsReady(context.Background(), deployment, service, 0)
	if err != nil || ready {
		t.Fatalf("zero replicas accepted live endpoints: %t, %v", ready, err)
	}
	for _, object := range []client.Object{pod, endpoints} {
		if err := suite.client.Delete(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	ready, err = suite.membershipEndpointsReady(context.Background(), deployment, service, 0)
	if err != nil || !ready {
		t.Fatalf("zero replicas did not accept an empty backend: %t, %v", ready, err)
	}
}

func membershipFixture(t *testing.T) (*openStackSuite, *appsv1.Deployment, *corev1.Service) {
	t.Helper()
	suite := selectedBackendTestSuite(t, selectedBackendTestNode("worker-a"))
	suite.config.RunID = "run-1234"
	suite.config.BackendReplicas = 1
	suite.config.BackendImage = "example.test/agnhost@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	suite.config.PollInterval = time.Millisecond
	deployment := suite.backendDeployment(1)
	deployment.UID = types.UID("deployment-uid")
	service := suite.backendService()
	service.UID = types.UID("service-uid")
	service.Spec.Ports[0].NodePort = 30001
	suite.backendDeploymentUID = deployment.UID
	suite.backendServiceUID = service.UID
	controller := true
	replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: suite.config.Namespace, Name: "backend-replicaset", UID: "replicaset-uid",
		Labels:          maps.Clone(service.Spec.Selector),
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", UID: deployment.UID, Controller: &controller}},
	}}
	for _, object := range []client.Object{deployment, service, replicaSet} {
		if err := suite.client.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	return suite, deployment, service
}

func membershipPodAndEndpoints(suite *openStackSuite, service *corev1.Service) (*corev1.Pod, *discoveryv1.EndpointSlice) {
	controller, ready := true, true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: suite.config.Namespace, Name: "backend-pod", UID: "pod-uid", Labels: maps.Clone(service.Spec.Selector),
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", UID: "replicaset-uid", Controller: &controller}},
		},
		Spec:   corev1.PodSpec{NodeName: "worker-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	endpoints := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: suite.config.Namespace, Name: "backend-endpoints", Labels: map[string]string{discoveryv1.LabelServiceName: backendName},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: service.UID, Controller: &controller}},
		},
		Endpoints: []discoveryv1.Endpoint{{
			NodeName: &pod.Spec.NodeName, Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
		}},
	}
	return pod, endpoints
}
