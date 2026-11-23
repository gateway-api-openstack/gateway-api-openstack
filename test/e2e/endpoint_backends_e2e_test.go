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
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// verifyEndpointBackends changes only test Services, EndpointSlices, and its
// HTTPRoute. The operator supplies the forward and return network paths.
func (s *openStackSuite) verifyEndpointBackends(ctx context.Context) error {
	if s.config.BackendMemberMode != configv1alpha1.MemberModeEndpoint {
		return fmt.Errorf("Endpoint backend checks require Endpoint member mode")
	}
	var route gatewayv1.HTTPRoute
	key := client.ObjectKey{Namespace: s.config.Namespace, Name: routeName}
	if err := s.client.Get(ctx, key, &route); err != nil {
		return err
	}
	original := route.Spec.DeepCopy()
	before, err := s.readOwnershipAudit(ctx, false)
	if err != nil {
		return err
	}
	endpoints, err := s.ownedBackendEndpoints(ctx)
	if err != nil {
		return err
	}
	for _, manual := range []bool{false, true} {
		service := s.backendService()
		service.Name = "headless-backend"
		service.Spec.ClusterIP = corev1.ClusterIPNone
		if manual {
			service.Name = "manual-backend"
			service.Spec.Selector = nil
			service.Spec.ClusterIP = ""
		}
		if err := s.client.Create(ctx, service); err != nil {
			return err
		}
		var manualSlice *discoveryv1.EndpointSlice
		if manual {
			manualSlice = endpointSliceForService(service, endpoints, s.config.RunID)
			if err := s.client.Create(ctx, manualSlice); err != nil {
				return err
			}
		}
		route.Spec.Rules[0].BackendRefs[0].Name = gatewayv1.ObjectName(service.Name)
		if err := s.patchRoutingRoute(ctx, key, s.routeUID, route.Spec); err != nil {
			return err
		}
		if err := s.waitForHTTPRoute(ctx); err != nil {
			return err
		}
		if err := s.verifyExternalTraffic(ctx); err != nil {
			return err
		}
		if err := s.waitEndpointMemberCount(ctx, len(endpoints)); err != nil {
			return err
		}
		if manual {
			// A terminating endpoint must leave the member set even while serving.
			initialReport, err := s.readOwnershipAudit(ctx, false)
			if err != nil {
				return err
			}
			initial, err := membershipFromAudit(initialReport, len(endpoints))
			if err != nil {
				return err
			}
			var retained map[string]struct{}
			base := manualSlice.DeepCopy()
			manualSlice.Endpoints[0].Conditions = discoveryv1.EndpointConditions{Ready: ptr.To(false), Serving: ptr.To(true), Terminating: ptr.To(true)}
			if err := s.client.Patch(ctx, manualSlice, controller.OptimisticMergeFrom(base)); err != nil {
				return err
			}
			if err := s.waitEndpointMemberCount(ctx, len(endpoints)-1); err != nil {
				return err
			}
			if len(endpoints) > 1 {
				drained, err := s.waitForMembershipInventory(ctx, len(endpoints)-1, initial, nil)
				if err != nil {
					return err
				}
				retained = drained.members
				if err := s.verifyExternalTraffic(ctx); err != nil {
					return err
				}
			}
			base = manualSlice.DeepCopy()
			manualSlice.Endpoints = cloneEndpoints(endpoints)
			if err := s.client.Patch(ctx, manualSlice, controller.OptimisticMergeFrom(base)); err != nil {
				return err
			}
			if err := s.waitEndpointMemberCount(ctx, len(endpoints)); err != nil {
				return err
			}
			if retained != nil {
				if _, err := s.waitForMembershipInventory(ctx, len(endpoints), initial, retained); err != nil {
					return err
				}
			}
			if err := s.verifyExternalTraffic(ctx); err != nil {
				return err
			}
		}
		if err := s.patchRoutingRoute(ctx, key, s.routeUID, *original); err != nil {
			return err
		}
		route.Spec = *original.DeepCopy()
		if err := s.waitForHTTPRoute(ctx); err != nil {
			return err
		}
		if err := s.verifyExternalTraffic(ctx); err != nil {
			return err
		}
		if manualSlice != nil {
			uid := manualSlice.UID
			if err := s.deleteMarkedAndWait(ctx, manualSlice, &uid); err != nil {
				return err
			}
		}
		uid := service.UID
		if err := s.deleteMarkedAndWait(ctx, service, &uid); err != nil {
			return err
		}
	}
	if err := s.verifyRoutingParentsUnchanged(ctx, before); err != nil {
		return err
	}
	return s.verifyActiveOwnershipInventoryAudit(ctx)
}

func endpointSliceForService(service *corev1.Service, endpoints []discoveryv1.Endpoint, runID string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: service.Namespace, Name: service.Name, Annotations: map[string]string{runAnnotation: runID}, Labels: map[string]string{discoveryv1.LabelServiceName: service.Name, discoveryv1.LabelManagedBy: "gateway-api-openstack-e2e"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("http"), Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(int32(backendPort))}},
		Endpoints:   cloneEndpoints(endpoints),
	}
}

func cloneEndpoints(endpoints []discoveryv1.Endpoint) []discoveryv1.Endpoint {
	cloned := make([]discoveryv1.Endpoint, len(endpoints))
	for i := range endpoints {
		endpoints[i].DeepCopyInto(&cloned[i])
	}
	return cloned
}

func (s *openStackSuite) ownedBackendEndpoints(ctx context.Context) ([]discoveryv1.Endpoint, error) {
	var service corev1.Service
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: backendName}, &service); err != nil {
		return nil, err
	}
	if service.UID != s.backendServiceUID || service.Annotations[runAnnotation] != s.config.RunID {
		return nil, fmt.Errorf("backend Service identity changed")
	}
	var items discoveryv1.EndpointSliceList
	if err := s.client.List(ctx, &items, client.InNamespace(s.config.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: backendName}); err != nil {
		return nil, err
	}
	endpoints := []discoveryv1.Endpoint{}
	seen := map[string]bool{}
	for _, slice := range items.Items {
		owner := metav1.GetControllerOf(&slice)
		if owner == nil || owner.Kind != "Service" || owner.UID != service.UID || slice.AddressType != discoveryv1.AddressTypeIPv4 {
			return nil, fmt.Errorf("backend EndpointSlice lacks the expected IPv4 Service ownership")
		}
		for _, endpoint := range slice.Endpoints {
			if !readyBackendEndpoint(endpoint) {
				continue
			}
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" || endpoint.TargetRef.Namespace != s.config.Namespace {
				return nil, fmt.Errorf("backend endpoint lacks its Pod identity")
			}
			var pod corev1.Pod
			if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: endpoint.TargetRef.Name}, &pod); err != nil {
				return nil, err
			}
			owner := metav1.GetControllerOf(&pod)
			if pod.UID != endpoint.TargetRef.UID || !podReady(&pod) || owner == nil || owner.Kind != "ReplicaSet" {
				return nil, fmt.Errorf("backend endpoint no longer identifies a ready test Pod")
			}
			var replicaSet appsv1.ReplicaSet
			if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: owner.Name}, &replicaSet); err != nil {
				return nil, err
			}
			deployment := metav1.GetControllerOf(&replicaSet)
			if replicaSet.UID != owner.UID || deployment == nil || deployment.Kind != "Deployment" || deployment.UID != s.backendDeploymentUID {
				return nil, fmt.Errorf("backend Pod is outside the test Deployment")
			}
			if len(endpoint.Addresses) != 1 || endpoint.Addresses[0] != pod.Status.PodIP {
				return nil, fmt.Errorf("backend endpoint does not match the test Pod address")
			}
			address, err := netip.ParseAddr(endpoint.Addresses[0])
			if err != nil || !address.Is4() || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() || address.IsLinkLocalUnicast() || seen[address.String()] {
				return nil, fmt.Errorf("backend endpoint address is invalid or duplicated")
			}
			seen[address.String()] = true
			endpoints = append(endpoints, *endpoint.DeepCopy())
		}
	}
	if len(endpoints) != int(s.config.BackendReplicas) {
		return nil, fmt.Errorf("backend EndpointSlice count does not match the approved replicas")
	}
	slices.SortFunc(endpoints, func(a, b discoveryv1.Endpoint) int { return cmp.Compare(a.Addresses[0], b.Addresses[0]) })
	return endpoints, nil
}

func (s *openStackSuite) waitEndpointMemberCount(ctx context.Context, expected int) error {
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		report, err := s.readOwnershipAudit(ctx, false)
		if err != nil {
			return false, err
		}
		members := 0
		for _, resource := range report.Resources {
			if resource.Service == "Octavia" && resource.Type == "Member" {
				members++
			}
		}
		return members == expected, nil
	})
}
