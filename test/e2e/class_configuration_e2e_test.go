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
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
)

// These classes share approved network IDs. Sequential Gateways keep the run
// within one load balancer at a time and do not claim multi-topology isolation.
func (s *openStackSuite) verifyFrozenClassConfiguration(ctx context.Context) error {
	before, beforeLease, beforeProcess, err := s.scrapeCurrentLeaderMetrics(ctx)
	if err != nil {
		return err
	}
	gateway, resolved, err := s.readGatewayTopology(ctx)
	if err != nil {
		return err
	}
	if err := topologyMatchesParameters(resolved, s.classConfig); err != nil {
		return err
	}
	s.frozenGateway = gateway
	if err := s.createSecondClass(ctx); err != nil {
		return err
	}
	if err := s.verifyRunScopedClassConfig(ctx); err != nil {
		return err
	}
	// Only change the run's own template. A larger delay reduces monitor load.
	updated := s.classConfig.DeepCopy()
	delay := *updated.Spec.HealthMonitor.Delay * 3
	updated.Spec.HealthMonitor.Delay = &delay
	if err := s.client.Update(ctx, updated); err != nil {
		return fmt.Errorf("update the test class template: %w", err)
	}
	s.classConfig = updated
	if err := s.verifyFrozenGateway(ctx); err != nil {
		return err
	}
	if err := s.rejectControllerIdentityCollision(ctx); err != nil {
		return err
	}
	var class gatewayv1.GatewayClass
	if err := s.client.Get(ctx, client.ObjectKey{Name: s.gatewayClassName}, &class); err != nil {
		return err
	}
	if class.UID != s.gatewayClassUID || class.Annotations[runAnnotation] != s.config.RunID ||
		class.Spec.ControllerName != gatewayv1.GatewayController(s.config.ControllerName) || !class.DeletionTimestamp.IsZero() ||
		class.Spec.ParametersRef == nil || class.Spec.ParametersRef.Name != s.classConfig.Name ||
		class.Spec.ParametersRef.Group != gatewayv1.Group(configv1alpha1.GroupName) || class.Spec.ParametersRef.Kind != gatewayv1.Kind(configv1alpha1.GatewayClassConfigKind) || class.Spec.ParametersRef.Namespace != nil {
		return fmt.Errorf("test GatewayClass identity changed before parameter reference update")
	}
	class.Spec.ParametersRef = s.secondClass.Spec.ParametersRef.DeepCopy()
	if err := s.client.Update(ctx, &class); err != nil {
		return fmt.Errorf("update the test class parameter reference: %w", err)
	}
	s.classParametersName = s.secondClassConfig.Name
	if err := s.waitForGatewayClass(ctx); err != nil {
		return err
	}
	if err := s.verifyFrozenGateway(ctx); err != nil {
		return err
	}
	after, afterLease, afterProcess, err := s.scrapeCurrentLeaderMetrics(ctx)
	if err != nil {
		return err
	}
	if beforeLease != afterLease || beforeProcess != afterProcess {
		return fmt.Errorf("controller leader changed during class configuration checks")
	}
	return validateConvergedNoOp(before, after, s.config.LeaderLease)
}

func (s *openStackSuite) verifyFrozenGatewayAfterRestart(ctx context.Context) error {
	if err := s.verifyFrozenGateway(ctx); err != nil {
		return err
	}
	// The cold restart stopped every controller Pod. Any mutation recorded by
	// its new leader would violate reconstruction from the frozen binding.
	snapshot, _, _, err := s.scrapeCurrentLeaderMetrics(ctx)
	if err != nil {
		return err
	}
	if snapshot.values[mutationCounterMetricName] != 0 || snapshot.values[mutationInFlightMetricName] != 0 {
		return fmt.Errorf("controller mutated the frozen graph after its cold restart")
	}
	return nil
}

func (s *openStackSuite) createSecondClass(ctx context.Context) error {
	name := s.config.GatewayClassName + "-second"
	for _, object := range []client.Object{&configv1alpha1.OpenStackGatewayClassConfig{}, &gatewayv1.GatewayClass{}} {
		if err := s.requireAbsent(ctx, client.ObjectKey{Name: name}, object); err != nil {
			return err
		}
	}
	parameters := s.classConfig.DeepCopy()
	parameters.ObjectMeta = metav1.ObjectMeta{Name: name, Annotations: map[string]string{runAnnotation: s.config.RunID}}
	delay := *parameters.Spec.HealthMonitor.Delay * 2
	parameters.Spec.HealthMonitor.Delay = &delay
	s.secondClassConfig = parameters
	if err := s.client.Create(ctx, parameters); err != nil {
		return fmt.Errorf("create second class parameters: %w", err)
	}
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{runAnnotation: s.config.RunID}},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(s.config.ControllerName),
			ParametersRef:  &gatewayv1.ParametersReference{Group: gatewayv1.Group(configv1alpha1.GroupName), Kind: gatewayv1.Kind(configv1alpha1.GatewayClassConfigKind), Name: name},
		},
	}
	s.secondClass = class
	if err := s.client.Create(ctx, class); err != nil {
		return fmt.Errorf("create second GatewayClass: %w", err)
	}
	return s.waitForClass(ctx, name)
}

func (s *openStackSuite) readGatewayTopology(ctx context.Context) (*gatewayv1.Gateway, classconfig.Resolved, error) {
	var gateway gatewayv1.Gateway
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: gatewayName}, &gateway); err != nil {
		return nil, classconfig.Resolved{}, err
	}
	if gateway.UID == "" || gateway.UID != s.gatewayUID || gateway.Annotations[runAnnotation] != s.config.RunID || !gateway.DeletionTimestamp.IsZero() {
		return nil, classconfig.Resolved{}, fmt.Errorf("test Gateway identity changed")
	}
	resolved, err := controller.StoredGatewayTopology(controller.Config{ControllerName: gatewayv1.GatewayController(s.config.ControllerName)}, &gateway)
	return &gateway, resolved, err
}

func topologyMatchesParameters(resolved classconfig.Resolved, parameters *configv1alpha1.OpenStackGatewayClassConfig) error {
	if parameters == nil {
		return fmt.Errorf("expected class settings are missing")
	}
	_, addressType, monitor, err := classconfig.Validate(parameters.Spec)
	if err != nil {
		return err
	}
	if parameters.UID == "" || resolved.ParametersName != parameters.Name ||
		resolved.ParametersUID != parameters.UID || resolved.ParametersGeneration != parameters.Generation ||
		resolved.Topology.VIPSubnetID != parameters.Spec.VIPSubnet.ID || resolved.Topology.MemberSubnetID != parameters.Spec.MemberSubnet.ID ||
		resolved.Topology.ExternalNetworkID != "" || resolved.Topology.FlavorID != parameters.Spec.FlavorID || resolved.Topology.AvailabilityZone != parameters.Spec.AvailabilityZone ||
		resolved.NodeAddressType != addressType || resolved.HealthMonitor != monitor {
		return fmt.Errorf("Gateway did not freeze the expected class settings")
	}
	return nil
}

func (s *openStackSuite) verifyFrozenGateway(ctx context.Context) error {
	if s.frozenGateway == nil {
		return fmt.Errorf("frozen Gateway baseline is missing")
	}
	// A semantically equivalent route edit waits for an evaluated generation,
	// rather than treating old Ready conditions as proof of reconciliation.
	if err := s.triggerConvergedRouteObservation(ctx); err != nil {
		return err
	}
	gateway, _, err := s.readGatewayTopology(ctx)
	if err != nil {
		return err
	}
	config := controller.Config{ControllerName: gatewayv1.GatewayController(s.config.ControllerName)}
	if !controller.GatewayTopologyMetadataEqual(config, s.frozenGateway, gateway) ||
		!equality.Semantic.DeepEqual(s.frozenGateway.Status.Addresses, gateway.Status.Addresses) {
		return fmt.Errorf("Gateway topology or address changed after a class template change")
	}
	var fingerprint [32]byte
	summary, err := s.runOwnershipAudit(ctx, false, &fingerprint)
	if err != nil {
		return err
	}
	if s.activeAudit == nil || summary != *s.activeAudit || fingerprint != s.activeAuditFingerprint {
		return fmt.Errorf("class change replaced resources in the existing Gateway graph")
	}
	return s.verifyExternalTraffic(ctx)
}

func (s *openStackSuite) verifySequentialClassConfiguration(ctx context.Context) error {
	if err := s.deleteHTTPRoute(ctx); err != nil {
		return err
	}
	if err := s.deleteGateway(ctx); err != nil {
		return err
	}
	// Finalizers and an independent empty inventory are both prerequisites to
	// creating the next Gateway, keeping peak OpenStack usage bounded.
	if err := wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		_, err := s.runOwnershipAudit(ctx, true, nil)
		return err == nil, nil
	}); err != nil {
		return fmt.Errorf("wait for the first Gateway graph to disappear: %w", err)
	}
	if err := s.verifyRunScopedControllerClasses(ctx); err != nil {
		return err
	}
	if err := s.createGatewayForClass(ctx, s.secondClass.Name); err != nil {
		return err
	}
	if err := s.createHTTPRoute(ctx); err != nil {
		return err
	}
	_, resolved, err := s.readGatewayTopology(ctx)
	if err != nil {
		return err
	}
	if err := topologyMatchesParameters(resolved, s.secondClassConfig); err != nil {
		return err
	}
	if err := s.verifyExternalTraffic(ctx); err != nil {
		return err
	}
	return s.verifyActiveOwnershipInventoryAudit(ctx)
}

func (s *openStackSuite) deleteSecondClass(ctx context.Context) error {
	var objects []client.Object
	if s.secondClass != nil {
		objects = append(objects, s.secondClass)
	}
	if s.secondClassConfig != nil {
		objects = append(objects, s.secondClassConfig)
	}
	for _, object := range objects {
		uid := object.GetUID()
		if uid == "" {
			return fmt.Errorf("second class creation had an unverified outcome; retain it for review")
		}
		if err := s.deleteMarkedAndWait(ctx, object, &uid); err != nil {
			return err
		}
	}
	s.secondClass, s.secondClassConfig = nil, nil
	return nil
}
