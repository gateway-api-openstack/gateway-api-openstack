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
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/audit"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/auditexec"
)

type membershipInventory struct {
	members map[string]struct{}
	parents [32]byte
}

func (s *openStackSuite) verifyWorkerMembershipTransitions(ctx context.Context) error {
	const replicas int32 = 2
	if err := s.validateMembershipWorkers(ctx); err != nil {
		return err
	}
	if err := s.waitForMembershipBackends(ctx, replicas); err != nil {
		return err
	}
	report, err := s.readOwnershipAudit(ctx, false)
	if err != nil {
		return err
	}
	baseline, err := membershipFromAudit(report, int(replicas))
	if err != nil {
		return err
	}
	address, err := s.gatewayAddress(ctx)
	if err != nil {
		return err
	}
	started := time.Now()
	before, beforeLease, beforeProcess, err := s.scrapeCurrentLeaderMetrics(ctx)
	if err != nil {
		return err
	}
	if err := validateMetricEndpointIdentity(before, s.config.LeaderLease); err != nil {
		return err
	}
	if _, found := before.values[requestStartsMetricName]; !found {
		return fmt.Errorf("controller metrics do not expose OpenStack request admissions")
	}
	samples, failures, transitionErr := observeTransitionTraffic(ctx, max(s.config.PollInterval, time.Second),
		func(ctx context.Context) bool { return s.probeExternalTraffic(ctx, address) },
		func() error { return s.transitionWorkerMembership(ctx, baseline) })
	evidence := membershipEvidence{TrafficSamples: samples, TrafficFailures: failures}
	s.report.Membership = &evidence
	if transitionErr != nil {
		return transitionErr
	}
	after, afterLease, afterProcess, err := s.scrapeCurrentLeaderMetrics(ctx)
	if err != nil {
		return err
	}
	if beforeLease.holder != afterLease.holder || beforeLease.transitions != afterLease.transitions || beforeProcess != afterProcess {
		return fmt.Errorf("controller leader changed during the membership observation")
	}
	mutations := 0
	if s.config.BackendExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
		mutations = 2 // Delete the departed member, then create its replacement.
	}
	evidence, err = membershipMetricEvidence(before, after, s.config.LeaderLease, time.Since(started), mutations)
	evidence.TrafficSamples, evidence.TrafficFailures = samples, failures
	return err
}

func (s *openStackSuite) validateMembershipWorkers(ctx context.Context) error {
	const replicas int32 = 2
	if s.config.BackendReplicas != replicas || len(s.config.BackendNodeSelector) == 0 {
		return fmt.Errorf("backend transition requires two replicas and an explicit worker selector")
	}
	nodes, err := s.selectedBackendNodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) != int(replicas) {
		return fmt.Errorf("backend transition requires exactly two selected eligible workers")
	}
	if s.config.BackendExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyCluster {
		// The current class API does not filter Cluster members by Node label.
		// Placement labels alone cannot authorize registering other workers.
		all, err := s.eligibleBackendNodes(ctx, nil)
		if err != nil {
			return err
		}
		if s.config.Project.Mode != projectModeDedicated || !maps.Equal(nodes, all) {
			return fmt.Errorf("Cluster membership transition requires a dedicated project with exactly the two selected eligible workers")
		}
	}
	return nil
}

func (s *openStackSuite) transitionWorkerMembership(ctx context.Context, baseline membershipInventory) error {
	if err := s.scaleBackendAndWait(ctx, 1); err != nil {
		return err
	}
	members := 1
	if s.config.BackendExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyCluster {
		members = len(baseline.members)
	}
	retained, err := s.waitForMembershipInventory(ctx, members, baseline, nil)
	if err != nil {
		return err
	}
	if err := s.verifyExternalTraffic(ctx); err != nil {
		return err
	}
	if err := s.scaleBackendAndWait(ctx, s.config.BackendReplicas); err != nil {
		return err
	}
	if _, err := s.waitForMembershipInventory(ctx, len(baseline.members), baseline, retained.members); err != nil {
		return err
	}
	return s.verifyWorkloadAfterRecovery(ctx)
}

// observeTransitionTraffic sends one request per interval without retrying a
// failed sample. It checks continuity only for a transition retaining a healthy
// backend, not for arbitrary Node removal or address replacement.
func observeTransitionTraffic(ctx context.Context, interval time.Duration, probe func(context.Context) bool, transition func() error) (int, int, error) {
	if !probe(ctx) {
		return 1, 1, fmt.Errorf("traffic failed before the membership transition")
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type observation struct{ samples, failures int }
	done := make(chan observation, 1)
	go func() {
		result := observation{samples: 1}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer func() { done <- result }()
		for {
			select {
			case <-probeCtx.Done():
				return
			case <-ticker.C:
				passed := probe(probeCtx)
				if probeCtx.Err() != nil {
					return
				}
				result.samples++
				if !passed {
					result.failures++
				}
			}
		}
	}()
	err := transition()
	cancel()
	result := <-done
	if ctx.Err() == nil {
		result.samples++
		if !probe(ctx) {
			result.failures++
		}
	}
	if result.failures > 0 {
		err = errors.Join(err, fmt.Errorf("traffic failed in %d of %d membership transition samples", result.failures, result.samples))
	}
	return result.samples, result.failures, errors.Join(err, ctx.Err())
}

func (s *openStackSuite) scaleBackendAndWait(ctx context.Context, replicas int32) error {
	if err := s.scaleBackend(ctx, replicas); err != nil {
		return err
	}
	return s.waitForMembershipBackends(ctx, replicas)
}

func (s *openStackSuite) scaleBackend(ctx context.Context, replicas int32) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, _, err := s.transitionBackendObjects(ctx)
		if err != nil {
			return err
		}
		base := deployment.DeepCopy()
		deployment.Spec.Replicas = &replicas
		return s.client.Patch(ctx, deployment, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func (s *openStackSuite) transitionBackendObjects(ctx context.Context) (*appsv1.Deployment, *corev1.Service, error) {
	key := client.ObjectKey{Namespace: s.config.Namespace, Name: backendName}
	var deployment appsv1.Deployment
	if err := s.client.Get(ctx, key, &deployment); err != nil {
		return nil, nil, fmt.Errorf("read backend Deployment before transition: %w", err)
	}
	var service corev1.Service
	if err := s.client.Get(ctx, key, &service); err != nil {
		return nil, nil, fmt.Errorf("read backend Service before transition: %w", err)
	}
	for _, object := range []struct {
		value client.Object
		uid   types.UID
	}{
		{value: &deployment, uid: s.backendDeploymentUID},
		{value: &service, uid: s.backendServiceUID},
	} {
		if object.uid == "" || object.value.GetUID() != object.uid || object.value.GetAnnotations()[runAnnotation] != s.config.RunID || !object.value.GetDeletionTimestamp().IsZero() {
			return nil, nil, fmt.Errorf("backend identity changed before transition")
		}
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas < 0 || *deployment.Spec.Replicas > s.config.BackendReplicas {
		return nil, nil, fmt.Errorf("backend replica count changed outside the test profile")
	}
	wanted := s.backendDeployment(*deployment.Spec.Replicas)
	if !backendDeploymentMatchesProfile(&deployment, wanted) {
		return nil, nil, fmt.Errorf("backend Deployment no longer matches its test profile")
	}
	if service.Spec.Type != corev1.ServiceTypeNodePort || service.Spec.ExternalTrafficPolicy != s.config.BackendExternalTrafficPolicy ||
		!maps.Equal(service.Spec.Selector, wanted.Spec.Template.Labels) || len(service.Spec.Ports) != 1 {
		return nil, nil, fmt.Errorf("backend Service no longer matches its selected worker profile")
	}
	port := service.Spec.Ports[0]
	if port.Name != "http" || port.Protocol != corev1.ProtocolTCP || port.Port != listenerPort || port.TargetPort != s.backendService().Spec.Ports[0].TargetPort || port.NodePort == 0 {
		return nil, nil, fmt.Errorf("backend Service port changed before transition")
	}
	return &deployment, &service, nil
}

func backendDeploymentMatchesProfile(deployment, wanted *appsv1.Deployment) bool {
	actualPod, wantedPod := deployment.Spec.Template.Spec, wanted.Spec.Template.Spec
	if !equality.Semantic.DeepEqual(deployment.Spec.Selector, wanted.Spec.Selector) ||
		!maps.Equal(deployment.Spec.Template.Labels, wanted.Spec.Template.Labels) ||
		!maps.Equal(actualPod.NodeSelector, wantedPod.NodeSelector) || !equality.Semantic.DeepEqual(actualPod.Affinity, wantedPod.Affinity) ||
		actualPod.NodeName != "" || actualPod.HostNetwork || actualPod.HostPID || actualPod.HostIPC ||
		len(actualPod.Containers) != 1 || len(actualPod.InitContainers) != 0 || len(actualPod.EphemeralContainers) != 0 {
		return false
	}
	actual, expected := actualPod.Containers[0], wantedPod.Containers[0]
	if actual.Name != expected.Name || actual.Image != expected.Image || len(actual.Command) != 0 ||
		!slices.Equal(actual.Args, expected.Args) || len(actual.Ports) != 1 {
		return false
	}
	port, wantedPort := actual.Ports[0], expected.Ports[0]
	if port.Name != wantedPort.Name || port.ContainerPort != wantedPort.ContainerPort || port.HostPort != 0 || port.HostIP != "" ||
		(port.Protocol != "" && port.Protocol != corev1.ProtocolTCP) {
		return false
	}
	probe := actual.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil || probe.PeriodSeconds != expected.ReadinessProbe.PeriodSeconds {
		return false
	}
	// Compare the handler we specify, allowing its HTTP scheme default. Probe
	// thresholds, timeouts, and other server-defaulted fields are not fixture inputs.
	handler, wantedHandler := probe.ProbeHandler.DeepCopy(), expected.ReadinessProbe.ProbeHandler.DeepCopy()
	if handler.HTTPGet.Scheme == "" {
		handler.HTTPGet.Scheme = corev1.URISchemeHTTP
	}
	if wantedHandler.HTTPGet.Scheme == "" {
		wantedHandler.HTTPGet.Scheme = corev1.URISchemeHTTP
	}
	return equality.Semantic.DeepEqual(handler, wantedHandler)
}

func (s *openStackSuite) waitForMembershipBackends(ctx context.Context, replicas int32) error {
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		deployment, service, err := s.transitionBackendObjects(ctx)
		if err != nil {
			return false, err
		}
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != replicas || !backendDeploymentReady(deployment, replicas) {
			return false, nil
		}
		return s.membershipEndpointsReady(ctx, deployment, service, replicas)
	})
}

func (s *openStackSuite) membershipEndpointsReady(ctx context.Context, deployment *appsv1.Deployment, service *corev1.Service, replicas int32) (bool, error) {
	options := []client.ListOption{client.InNamespace(s.config.Namespace), client.MatchingLabels(service.Spec.Selector)}
	var replicaSets appsv1.ReplicaSetList
	if err := s.client.List(ctx, &replicaSets, options...); err != nil {
		return false, err
	}
	ownedSets := make(map[types.UID]bool, len(replicaSets.Items))
	for i := range replicaSets.Items {
		owner := metav1.GetControllerOf(&replicaSets.Items[i])
		if owner != nil && owner.Kind == "Deployment" && owner.UID == deployment.UID {
			ownedSets[replicaSets.Items[i].UID] = true
		}
	}
	var pods corev1.PodList
	if err := s.client.List(ctx, &pods, options...); err != nil {
		return false, err
	}
	selectedNodes, err := s.selectedBackendNodes(ctx)
	if err != nil {
		return false, err
	}
	readyPods := make(map[types.UID]*corev1.Pod, len(pods.Items))
	nodes := make(map[string]struct{}, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != "ReplicaSet" || !ownedSets[owner.UID] {
			return false, fmt.Errorf("backend selector includes a Pod outside the test Deployment")
		}
		if !podReady(pod) {
			return false, nil
		}
		if _, selected := selectedNodes[pod.Spec.NodeName]; !selected {
			return false, fmt.Errorf("backend Pod is not on a selected eligible worker")
		}
		readyPods[pod.UID] = pod
		nodes[pod.Spec.NodeName] = struct{}{}
	}
	if len(readyPods) != int(replicas) || len(nodes) != int(replicas) {
		return false, nil
	}
	var slices discoveryv1.EndpointSliceList
	if err := s.client.List(ctx, &slices, client.InNamespace(s.config.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: backendName}); err != nil {
		return false, err
	}
	observed := make(map[types.UID]bool, len(readyPods))
	for i := range slices.Items {
		slice := &slices.Items[i]
		owner := metav1.GetControllerOf(slice)
		if owner == nil || owner.Kind != "Service" || owner.UID != service.UID {
			return false, fmt.Errorf("backend EndpointSlice does not belong to the test Service")
		}
		for _, endpoint := range slice.Endpoints {
			if !readyBackendEndpoint(endpoint) {
				continue
			}
			ref := endpoint.TargetRef
			if ref == nil || ref.Kind != "Pod" || ref.Namespace != s.config.Namespace || endpoint.NodeName == nil {
				return false, fmt.Errorf("backend endpoint lacks its Pod and Node identity")
			}
			pod := readyPods[ref.UID]
			if pod == nil || ref.Name != pod.Name || *endpoint.NodeName != pod.Spec.NodeName || observed[ref.UID] {
				return false, nil
			}
			observed[ref.UID] = true
		}
	}
	return len(observed) == int(replicas), nil
}

func membershipFromAudit(report audit.Report, expected int) (membershipInventory, error) {
	if report.Summary.KubernetesBindings != 2 || report.Summary.OpenStackResources != len(report.Resources) || report.Summary.Matched != len(report.Resources) {
		return membershipInventory{}, fmt.Errorf("membership audit does not describe the expected Gateway and HTTPRoute: %w", auditexec.ErrInvalidReport)
	}
	inventory := membershipInventory{members: make(map[string]struct{}, expected)}
	parents := make([]audit.ResourceFinding, 0, len(report.Resources))
	for _, resource := range report.Resources {
		if resource.Service == "Octavia" && resource.Type == "Member" {
			if resource.ID == "" || resource.ParentID == "" {
				return membershipInventory{}, fmt.Errorf("membership audit contains an incomplete member identity: %w", auditexec.ErrInvalidReport)
			}
			if _, duplicate := inventory.members[resource.ID]; duplicate {
				return membershipInventory{}, fmt.Errorf("membership audit repeats a member identity: %w", auditexec.ErrInvalidReport)
			}
			inventory.members[resource.ID] = struct{}{}
			continue
		}
		parents = append(parents, resource)
	}
	if len(parents) == 0 {
		return membershipInventory{}, fmt.Errorf("membership audit lacks the parent graph: %w", auditexec.ErrInvalidReport)
	}
	if len(inventory.members) != expected {
		return membershipInventory{}, errInventoryPending
	}
	inventory.parents = ownershipAuditFingerprint(parents)
	return inventory, nil
}

func (s *openStackSuite) waitForMembershipInventory(ctx context.Context, expected int, baseline membershipInventory, retained map[string]struct{}) (membershipInventory, error) {
	var observed membershipInventory
	err := waitForOwnershipInventory(ctx, s.config.PollInterval, func(ctx context.Context) error {
		report, err := s.readOwnershipAudit(ctx, false)
		if err != nil {
			return err
		}
		observed, err = membershipFromAudit(report, expected)
		if err != nil {
			return err
		}
		if !membershipTransitionMatches(baseline, observed, retained) {
			return errInventoryPending
		}
		return nil
	})
	return observed, err
}

func membershipTransitionMatches(baseline, observed membershipInventory, retained map[string]struct{}) bool {
	if baseline.parents != observed.parents {
		return false
	}
	if retained == nil {
		// Scale-down may remove members, but must not recreate a surviving one.
		for id := range observed.members {
			if _, found := baseline.members[id]; !found {
				return false
			}
		}
		return true
	}
	for id := range retained {
		if _, found := observed.members[id]; !found {
			return false
		}
	}
	return true
}
