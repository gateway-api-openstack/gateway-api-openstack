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

package diagnostic

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func waitForBackend(ctx context.Context, c client.Reader, cfg Config, objects []client.Object, addressType corev1.NodeAddressType) error {
	var service *corev1.Service
	var deployment *appsv1.Deployment
	for _, object := range objects {
		switch value := object.(type) {
		case *corev1.Service:
			service = value
		case *appsv1.Deployment:
			deployment = value
		}
	}
	if service == nil || deployment == nil {
		return fmt.Errorf("diagnostic backend fixture is incomplete")
	}
	if err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		return backendReady(ctx, c, cfg, service, deployment, addressType)
	}); err != nil {
		return fmt.Errorf("wait for the selected worker backend before creating a Gateway: %w", err)
	}
	return nil
}

func backendReady(ctx context.Context, reader client.Reader, cfg Config, service *corev1.Service, deployment *appsv1.Deployment, addressType corev1.NodeAddressType) (bool, error) {
	var actualService corev1.Service
	if err := reader.Get(ctx, client.ObjectKeyFromObject(service), &actualService); err != nil {
		return false, err
	}
	if !actualService.DeletionTimestamp.IsZero() || actualService.UID != service.UID || actualService.Spec.Type != corev1.ServiceTypeNodePort || actualService.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal || len(actualService.Spec.Ports) != 1 || actualService.Spec.Ports[0].NodePort == 0 {
		return false, fmt.Errorf("diagnostic backend Service changed")
	}
	var actualDeployment appsv1.Deployment
	if err := reader.Get(ctx, client.ObjectKeyFromObject(deployment), &actualDeployment); err != nil {
		return false, err
	}
	if !actualDeployment.DeletionTimestamp.IsZero() || actualDeployment.UID != deployment.UID || !reflect.DeepEqual(actualDeployment.Spec.Template.Spec.NodeSelector, cfg.BackendNodeSelector) || actualDeployment.Spec.Replicas == nil || *actualDeployment.Spec.Replicas != 1 {
		return false, fmt.Errorf("diagnostic backend Deployment changed")
	}
	if actualDeployment.Status.ObservedGeneration != actualDeployment.Generation || actualDeployment.Status.ReadyReplicas != 1 {
		return false, nil
	}
	var slices discoveryv1.EndpointSliceList
	if err := reader.List(ctx, &slices, client.InNamespace(cfg.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: backendName}); err != nil {
		return false, err
	}
	readyPods := make(map[types.UID]struct{})
	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
				continue
			}
			if endpoint.NodeName == nil || endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" || endpoint.TargetRef.Namespace != cfg.Namespace {
				return false, nil
			}
			var pod corev1.Pod
			if err := reader.Get(ctx, client.ObjectKey{Name: endpoint.TargetRef.Name, Namespace: cfg.Namespace}, &pod); err != nil {
				return false, err
			}
			if pod.UID == "" || pod.UID != endpoint.TargetRef.UID || pod.Spec.NodeName != *endpoint.NodeName || !pod.DeletionTimestamp.IsZero() {
				return false, fmt.Errorf("backend EndpointSlice does not identify the actual scheduled Pod")
			}
			owner := metav1.GetControllerOf(&pod)
			if owner == nil || owner.Kind != "ReplicaSet" {
				return false, fmt.Errorf("backend Pod is not controlled by the diagnostic Deployment")
			}
			var replicaSet appsv1.ReplicaSet
			if err := reader.Get(ctx, client.ObjectKey{Name: owner.Name, Namespace: cfg.Namespace}, &replicaSet); err != nil {
				return false, err
			}
			deploymentOwner := metav1.GetControllerOf(&replicaSet)
			if replicaSet.UID != owner.UID || deploymentOwner == nil || deploymentOwner.Kind != "Deployment" || deploymentOwner.UID != deployment.UID {
				return false, fmt.Errorf("backend Pod belongs to a different Deployment")
			}
			podReady := false
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					podReady = true
				}
			}
			if !podReady {
				return false, nil
			}
			var node corev1.Node
			if err := reader.Get(ctx, client.ObjectKey{Name: *endpoint.NodeName}, &node); err != nil {
				return false, err
			}
			if controller.NodePortNodeExcluded(&node) || node.Spec.Unschedulable || !node.DeletionTimestamp.IsZero() || !labels.SelectorFromSet(cfg.BackendNodeSelector).Matches(labels.Set(node.Labels)) {
				return false, fmt.Errorf("actual backend endpoint is not on an eligible selected worker")
			}
			ready, address := false, false
			for _, condition := range node.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			for _, candidate := range node.Status.Addresses {
				if candidate.Type == addressType && net.ParseIP(candidate.Address) != nil {
					address = true
				}
			}
			if !ready || !address {
				return false, nil
			}
			readyPods[pod.UID] = struct{}{}
		}
	}
	return len(readyPods) == 1, nil
}
