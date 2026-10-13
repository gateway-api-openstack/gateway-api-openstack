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

package controller

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
)

// GatewayTopologyAnnotation records the resolved class configuration in the
// durable Gateway binding. It contains infrastructure IDs, never credentials.
func (c Config) GatewayTopologyAnnotation() string {
	return c.Domain() + "/gateway-topology-" + c.controllerKey()
}

// GatewayTopologyFinalizerPrefix identifies the topology integrity finalizer.
func (c Config) GatewayTopologyFinalizerPrefix() string {
	return c.Domain() + "/gateway-topology-" + c.controllerKey() + "-"
}

// GatewayTopologyFinalizer binds the exact serialized topology to metadata
// persisted before cloud creation. Like other binding finalizers, this detects
// accidental edits but is not a security boundary against finalizer editors.
func (c Config) GatewayTopologyFinalizer(value string) string {
	digest := sha256.Sum256([]byte(value))
	return c.GatewayTopologyFinalizerPrefix() + fmt.Sprintf("%x", digest[:16])
}

// BindGatewayTopology records a new resolved topology and its integrity
// finalizer. A bound Gateway never changes its infrastructure through this helper.
// The caller owns the object and must persist the patch before cloud mutation.
func BindGatewayTopology(config Config, gateway *gatewayv1.Gateway, resolved classconfig.Resolved) error {
	if err := validateStoredTopology(resolved); err != nil {
		return err
	}
	if HasGatewayTopologyBinding(config, gateway) {
		existing, err := StoredGatewayTopology(config, gateway)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(existing, resolved) {
			return fmt.Errorf("%w: Gateway topology binding is immutable", cloud.ErrOwnershipConflict)
		}
		return nil
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		return fmt.Errorf("encode Gateway topology: %w", err)
	}
	if gateway.Annotations == nil {
		gateway.Annotations = map[string]string{}
	}
	gateway.Annotations[config.GatewayTopologyAnnotation()] = string(encoded)
	controllerutil.AddFinalizer(gateway, config.GatewayTopologyFinalizer(string(encoded)))
	return nil
}

// StoredGatewayTopology validates the complete persisted topology without
// reading a GatewayClass, parameter object, or cloud resource.
func StoredGatewayTopology(config Config, gateway *gatewayv1.Gateway) (classconfig.Resolved, error) {
	raw := gateway.Annotations[config.GatewayTopologyAnnotation()]
	finalizers := topologyFinalizers(config, gateway)
	if raw == "" || len(finalizers) != 1 || finalizers[0] != config.GatewayTopologyFinalizer(raw) {
		return classconfig.Resolved{}, fmt.Errorf("%w: Gateway topology binding is missing, incomplete, or changed", cloud.ErrOwnershipConflict)
	}
	var resolved classconfig.Resolved
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&resolved); err != nil {
		return classconfig.Resolved{}, fmt.Errorf("%w: Gateway topology binding is invalid", cloud.ErrOwnershipConflict)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return classconfig.Resolved{}, fmt.Errorf("%w: Gateway topology binding contains trailing data", cloud.ErrOwnershipConflict)
	}
	if err := validateStoredTopology(resolved); err != nil {
		return classconfig.Resolved{}, err
	}
	return resolved, nil
}

// HasGatewayTopologyBinding detects any component of the persisted topology.
func HasGatewayTopologyBinding(config Config, gateway *gatewayv1.Gateway) bool {
	_, exists := gateway.Annotations[config.GatewayTopologyAnnotation()]
	return exists || len(topologyFinalizers(config, gateway)) != 0
}

// GatewayTopologyMetadataEqual compares only this controller's topology binding.
func GatewayTopologyMetadataEqual(config Config, left, right *gatewayv1.Gateway) bool {
	leftValue, leftExists := left.Annotations[config.GatewayTopologyAnnotation()]
	rightValue, rightExists := right.Annotations[config.GatewayTopologyAnnotation()]
	return leftExists == rightExists && leftValue == rightValue && slices.Equal(topologyFinalizers(config, left), topologyFinalizers(config, right))
}

// ClearGatewayTopology removes only the topology binding after owned resources
// are confirmed absent. The caller persists this with its finalizer patch.
func ClearGatewayTopology(config Config, gateway *gatewayv1.Gateway) {
	delete(gateway.Annotations, config.GatewayTopologyAnnotation())
	for _, finalizer := range topologyFinalizers(config, gateway) {
		controllerutil.RemoveFinalizer(gateway, finalizer)
	}
}

func topologyFinalizers(config Config, gateway *gatewayv1.Gateway) []string {
	var values []string
	for _, finalizer := range gateway.Finalizers {
		if strings.HasPrefix(finalizer, config.GatewayTopologyFinalizerPrefix()) {
			values = append(values, finalizer)
		}
	}
	slices.Sort(values)
	return values
}

func validateStoredTopology(resolved classconfig.Resolved) error {
	if resolved.ParametersName == "" || resolved.ParametersUID == "" || resolved.ParametersGeneration < 1 {
		return fmt.Errorf("%w: Gateway topology has an incomplete parameter identity", cloud.ErrOwnershipConflict)
	}
	monitor := resolved.HealthMonitor
	delay, timeout, retries, retriesDown := int32(monitor.Delay), int32(monitor.Timeout), int32(monitor.MaxRetries), int32(monitor.MaxRetriesDown)
	spec := configv1alpha1.OpenStackGatewayClassConfigSpec{
		VIPSubnet:       configv1alpha1.ResourceReference{ID: resolved.Topology.VIPSubnetID},
		MemberSubnet:    configv1alpha1.ResourceReference{ID: resolved.Topology.MemberSubnetID},
		NodeAddressType: string(resolved.NodeAddressType), FlavorID: resolved.Topology.FlavorID, AvailabilityZone: resolved.Topology.AvailabilityZone,
		HealthMonitor: configv1alpha1.HealthMonitorConfig{Path: monitor.Path, Delay: &delay, Timeout: &timeout, MaxRetries: &retries, MaxRetriesDown: &retriesDown},
	}
	if resolved.Topology.ExternalNetworkID != "" {
		spec.FloatingIP = configv1alpha1.FloatingIPConfig{Mode: configv1alpha1.FloatingIPAllocate, ExternalNetwork: &configv1alpha1.ResourceReference{ID: resolved.Topology.ExternalNetworkID}}
	}
	_, addressType, validatedMonitor, err := classconfig.Validate(spec)
	if err != nil || addressType != resolved.NodeAddressType || validatedMonitor != monitor {
		return fmt.Errorf("%w: Gateway topology contains an invalid resolved configuration", cloud.ErrOwnershipConflict)
	}
	return nil
}
