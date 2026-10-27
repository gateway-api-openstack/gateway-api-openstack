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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func TestNodePortMembersWorkerEligibility(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		mutate  func(*corev1.Node)
		include bool
	}{
		{name: "unlabelled worker", include: true},
		{name: "worker label", labels: map[string]string{"node-role.kubernetes.io/worker": ""}, include: true},
		{name: "control plane", labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
		{name: "control plane false value", labels: map[string]string{"node-role.kubernetes.io/control-plane": "false"}},
		{name: "legacy master", labels: map[string]string{"node-role.kubernetes.io/master": ""}},
		{name: "mixed roles", labels: map[string]string{"node-role.kubernetes.io/control-plane": "", "node-role.kubernetes.io/worker": ""}},
		{name: "control plane overrides inclusion", labels: map[string]string{"node-role.kubernetes.io/control-plane": "", corev1.LabelNodeExcludeBalancers: "false"}},
		{name: "empty exclusion", labels: map[string]string{corev1.LabelNodeExcludeBalancers: ""}},
		{name: "true exclusion", labels: map[string]string{corev1.LabelNodeExcludeBalancers: "true"}},
		{name: "false exclusion", labels: map[string]string{corev1.LabelNodeExcludeBalancers: "false"}, include: true},
		{name: "invalid exclusion", labels: map[string]string{corev1.LabelNodeExcludeBalancers: "invalid"}},
		{name: "numeric exclusion", labels: map[string]string{corev1.LabelNodeExcludeBalancers: "1"}},
		{name: "numeric inclusion", labels: map[string]string{corev1.LabelNodeExcludeBalancers: "0"}, include: true},
		{name: "uppercase inclusion", labels: map[string]string{corev1.LabelNodeExcludeBalancers: "FALSE"}, include: true},
		{name: "not ready", mutate: func(node *corev1.Node) { node.Status.Conditions[0].Status = corev1.ConditionFalse }},
		{name: "unknown readiness", mutate: func(node *corev1.Node) { node.Status.Conditions[0].Status = corev1.ConditionUnknown }},
		{name: "no readiness", mutate: func(node *corev1.Node) { node.Status.Conditions = nil }},
		{name: "unschedulable", mutate: func(node *corev1.Node) { node.Spec.Unschedulable = true }},
		{name: "deleting", mutate: func(node *corev1.Node) {
			now := metav1.Now()
			node.DeletionTimestamp = &now
			node.Finalizers = []string{"example.com/test"}
		}},
		{name: "no selected address", mutate: func(node *corev1.Node) { node.Status.Addresses[0].Type = corev1.NodeExternalIP }},
		{name: "invalid address", mutate: func(node *corev1.Node) { node.Status.Addresses[0].Address = "invalid" }},
	}
	for _, policy := range []corev1.ServiceExternalTrafficPolicy{corev1.ServiceExternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyLocal} {
		t.Run(string(policy), func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					service, endpoints, node := validNodePortBackend("default", "backend")
					service.Spec.ExternalTrafficPolicy = policy
					other := node.DeepCopy()
					other.Name = "worker-1"
					other.Status.Addresses[0].Address = "192.0.2.20"
					endpoints.Endpoints = append(endpoints.Endpoints, discoveryv1.Endpoint{
						NodeName: ptr.To(other.Name), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
					})
					node.Labels = tt.labels
					if tt.mutate != nil {
						tt.mutate(node)
					}
					kubeClient := indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(endpoints, node, other).Build()

					members, err := nodePortMembersFromReader(context.Background(), kubeClient, service, 30080, corev1.NodeInternalIP)
					if err != nil {
						t.Fatal(err)
					}
					want := []cloud.Member{{Address: "192.0.2.20", Port: 30080}}
					if tt.include {
						want = append([]cloud.Member{{Address: "192.0.2.10", Port: 30080}}, want...)
					}
					if !slices.Equal(members, want) {
						t.Fatalf("members = %#v, want %#v", members, want)
					}
				})
			}
		})
	}
}

func TestNodePortMembersNoEligibleWorker(t *testing.T) {
	for _, policy := range []corev1.ServiceExternalTrafficPolicy{corev1.ServiceExternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyLocal} {
		t.Run(string(policy), func(t *testing.T) {
			service, endpoints, node := validNodePortBackend("default", "backend")
			service.Spec.ExternalTrafficPolicy = policy
			node.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
			kubeClient := indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(endpoints, node).Build()

			members, err := nodePortMembersFromReader(context.Background(), kubeClient, service, 30080, corev1.NodeInternalIP)
			var buildErr *routeBuildError
			if !errors.As(err, &buildErr) || buildErr.kind != routeErrorPending || len(members) != 0 {
				t.Fatalf("members = %#v, error = %v, want pending without members", members, err)
			}
		})
	}
}

func TestNodePortMembersRejectConflictingProviderIDs(t *testing.T) {
	for _, test := range []struct {
		name       string
		providerID string
		conflict   bool
	}{
		{name: "same instance", providerID: "openstack:///server-a"},
		{name: "different instance", providerID: "openstack:///server-b", conflict: true},
		{name: "missing identity", conflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, first := validNodePortBackend("default", "backend")
			first.Spec.ProviderID = "openstack:///server-a"
			second := first.DeepCopy()
			second.Name = "second-worker"
			second.Spec.ProviderID = test.providerID
			nodes := []corev1.Node{*first, *second}
			for range 2 {
				members, err := nodePortMembersForNodes(service, 30080, corev1.NodeInternalIP, map[string]struct{}{"": {}}, nodes)
				if test.conflict {
					var buildError *routeBuildError
					if !errors.As(err, &buildError) || buildError.kind != routeErrorPending || len(members) != 0 {
						t.Fatalf("ambiguous provider identity produced members: %v, %v", members, err)
					}
				} else if err != nil || len(members) != 1 || members[0].ProviderID != first.Spec.ProviderID {
					t.Fatalf("identical member tuples did not deduplicate: %v, %v", members, err)
				}
				slices.Reverse(nodes)
			}
		})
	}
}

func TestNodePortMembersFollowWorkerLabelChanges(t *testing.T) {
	for _, policy := range []corev1.ServiceExternalTrafficPolicy{corev1.ServiceExternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyLocal} {
		t.Run(string(policy), func(t *testing.T) {
			service, endpoints, node := validNodePortBackend("default", "backend")
			service.Spec.ExternalTrafficPolicy = policy
			other := node.DeepCopy()
			other.Name = "worker-1"
			other.Status.Addresses[0].Address = "192.0.2.20"
			endpoints.Endpoints = append(endpoints.Endpoints, discoveryv1.Endpoint{
				NodeName: ptr.To(other.Name), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			})
			kubeClient := indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(endpoints, node, other).Build()

			for _, step := range []struct {
				labels  map[string]string
				include bool
			}{
				{include: true},
				{labels: map[string]string{corev1.LabelNodeExcludeBalancers: ""}},
				{labels: map[string]string{corev1.LabelNodeExcludeBalancers: "false"}, include: true},
				{labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
				{include: true},
			} {
				node.Labels = step.labels
				if err := kubeClient.Update(context.Background(), node); err != nil {
					t.Fatal(err)
				}
				members, err := nodePortMembersFromReader(context.Background(), kubeClient, service, 30080, corev1.NodeInternalIP)
				if err != nil {
					t.Fatal(err)
				}
				want := []cloud.Member{{Address: "192.0.2.20", Port: 30080}}
				if step.include {
					want = append([]cloud.Member{{Address: "192.0.2.10", Port: 30080}}, want...)
				}
				if !slices.Equal(members, want) {
					t.Fatalf("labels = %#v, members = %#v, want %#v", step.labels, members, want)
				}
			}
		})
	}
}
