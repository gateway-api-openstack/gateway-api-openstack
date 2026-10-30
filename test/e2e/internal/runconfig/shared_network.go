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

package runconfig

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

func (c Runtime) validateSharedNetworkTests() error {
	if !c.Project.AllowSharedNetworkTests {
		if len(c.ExpectedWorkers) != 0 {
			return fmt.Errorf("backend.expectedWorkers requires openstack.allowSharedNetworkTests")
		}
		return nil
	}
	if c.Project.Mode != ProjectModeShared || c.LocalLifecycle || !c.MemberTransitions ||
		c.BackendExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster || c.BackendReplicas != 2 || len(c.BackendNodeSelector) == 0 {
		return fmt.Errorf("shared network tests require a single Cluster lifecycle with memberTransitions, two replicas, and an explicit worker selector")
	}
	if c.ExpectedAPIServer == "" || c.ExpectedClusterUID == "" {
		return fmt.Errorf("shared network tests require exact Kubernetes cluster identity")
	}
	if len(c.ExpectedWorkers) != 2 {
		return fmt.Errorf("shared network tests require exactly two backend.expectedWorkers")
	}
	names, uids, providers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, worker := range c.ExpectedWorkers {
		if len(validation.IsDNS1123Subdomain(worker.Name)) != 0 || worker.UID == "" || strings.TrimSpace(worker.UID) != worker.UID ||
			!strings.HasPrefix(worker.ProviderID, "openstack://") || strings.TrimPrefix(worker.ProviderID, "openstack://") == "" ||
			strings.ContainsAny(worker.ProviderID, " \t\r\n") {
			return fmt.Errorf("each expected worker requires its Node name, UID, and OpenStack provider ID")
		}
		if names[worker.Name] || uids[worker.UID] || providers[worker.ProviderID] {
			return fmt.Errorf("expected workers must have distinct Node and provider identities")
		}
		names[worker.Name], uids[worker.UID], providers[worker.ProviderID] = true, true, true
	}
	return nil
}

// RequireSharedNetworkWorkers checks the approved worker identities before a
// shared network test. This observation does not prevent concurrent Node changes;
// operators must keep the dedicated test cluster's worker set stable during a run.
func RequireSharedNetworkWorkers(ctx context.Context, reader client.Reader, config Runtime) error {
	if err := config.validateSharedNetworkTests(); err != nil {
		return err
	}
	if !config.Project.AllowSharedNetworkTests {
		return nil
	}
	if err := RequireClusterIdentity(ctx, reader, config); err != nil {
		return err
	}
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return fmt.Errorf("list Nodes for shared network test: %w", err)
	}
	expected := make(map[string]WorkerIdentity, len(config.ExpectedWorkers))
	for _, worker := range config.ExpectedWorkers {
		expected[worker.Name] = worker
	}
	selector := labels.SelectorFromSet(config.BackendNodeSelector)
	matched := 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if controller.NodePortNodeExcluded(node) {
			continue
		}
		// Include unready and unschedulable workers: either can become eligible
		// while the test controller is reconciling the Cluster member set.
		worker, approved := expected[node.Name]
		if !approved || string(node.UID) != worker.UID || node.Spec.ProviderID != worker.ProviderID ||
			!node.DeletionTimestamp.IsZero() || node.Spec.Unschedulable || !selector.Matches(labels.Set(node.Labels)) {
			return fmt.Errorf("worker identity or placement differs from the approved shared network test")
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				ready = condition.Status == corev1.ConditionTrue
			}
		}
		if !ready {
			return fmt.Errorf("an approved shared network test worker is not ready")
		}
		matched++
	}
	if matched != len(expected) {
		return fmt.Errorf("shared network test requires exactly the two approved workers")
	}
	return nil
}
