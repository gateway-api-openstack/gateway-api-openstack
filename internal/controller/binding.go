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
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

// ValidateGatewayBinding verifies the complete durable binding before lifecycle work.
func ValidateGatewayBinding(config Config, gateway *gatewayv1.Gateway) error {
	if err := ValidateGatewayIdentityBinding(config, gateway); err != nil {
		return err
	}
	if GatewayHasControllerBinding(config, gateway) {
		if _, err := StoredGatewayTopology(config, gateway); err != nil {
			return err
		}
	}
	return nil
}

// ValidateGatewayIdentityBinding validates only the immutable ownership tuple.
// The advisory audit does not need provisioning parameters to inventory identity.
// This check alone never authorizes graph mutation or finalizer removal.
func ValidateGatewayIdentityBinding(config Config, gateway *gatewayv1.Gateway) error {
	bindingValues := []string{
		gateway.Annotations[config.GatewayClusterIDAnnotation()],
		gateway.Annotations[config.GatewayProjectIDAnnotation()],
	}
	populatedBindingCount := 0
	for _, value := range bindingValues {
		if value != "" {
			populatedBindingCount++
		}
	}
	if populatedBindingCount != len(bindingValues) && (populatedBindingCount != 0 || controllerutil.ContainsFinalizer(gateway, config.GatewayFinalizer()) || HasGatewayTopologyBinding(config, gateway)) {
		return fmt.Errorf("gateway %s/%s has an incomplete stored OpenStack resource binding; restore the original annotations before reconciling", gateway.Namespace, gateway.Name)
	}
	if stored := gateway.Annotations[config.GatewayClusterIDAnnotation()]; stored != "" && stored != config.ClusterID {
		return fmt.Errorf("controller cluster identity differs from this Gateway's existing resource binding; restore the original cluster ID before reconciling")
	}
	if stored := gateway.Annotations[config.GatewayProjectIDAnnotation()]; stored != "" && stored != config.OpenStackProjectID {
		return fmt.Errorf("authenticated OpenStack project differs from this Gateway's existing resource binding; restore access to the original project before reconciling")
	}
	return nil
}

// GatewayHasControllerBinding reports whether the Gateway contains any part
// of this controller's durable binding.
func GatewayHasControllerBinding(config Config, gateway *gatewayv1.Gateway) bool {
	if controllerutil.ContainsFinalizer(gateway, config.GatewayFinalizer()) || HasGatewayTopologyBinding(config, gateway) {
		return true
	}
	for _, key := range []string{
		config.GatewayClusterIDAnnotation(),
		config.GatewayProjectIDAnnotation(),
	} {
		if gateway.Annotations[key] != "" {
			return true
		}
	}
	return false
}

// GatewayBindingMetadataEqual compares only Gateway metadata owned by this
// controller.
func GatewayBindingMetadataEqual(config Config, left, right *gatewayv1.Gateway) bool {
	if !GatewayTopologyMetadataEqual(config, left, right) {
		return false
	}
	if controllerutil.ContainsFinalizer(left, config.GatewayFinalizer()) !=
		controllerutil.ContainsFinalizer(right, config.GatewayFinalizer()) {
		return false
	}
	for _, key := range []string{
		config.GatewayClusterIDAnnotation(),
		config.GatewayProjectIDAnnotation(),
	} {
		if left.Annotations[key] != right.Annotations[key] {
			return false
		}
	}
	return true
}

// StoredGatewayIdentity returns the Gateway identity using durable binding
// values when they are present.
func StoredGatewayIdentity(config Config, gateway *gatewayv1.Gateway) cloud.Identity {
	identity := GatewayIdentity(config, gateway)
	if value := gateway.Annotations[config.GatewayClusterIDAnnotation()]; value != "" {
		identity.ClusterID = value
	}
	if value := gateway.Annotations[config.GatewayProjectIDAnnotation()]; value != "" {
		identity.OpenStackProjectID = value
	}
	return identity
}

// SameGatewayIdentity compares the Gateway portion of two cloud identities.
func SameGatewayIdentity(left, right cloud.Identity) bool {
	return left.GatewayNamespace == right.GatewayNamespace &&
		left.GatewayName == right.GatewayName &&
		left.GatewayUID == right.GatewayUID
}

// RouteBinding records one HTTPRoute fragment's durable Gateway ownership.
// Identity includes the Route UID so copying annotations cannot transfer ownership.
type RouteBinding struct {
	Identity       cloud.Identity `json:"identity"`
	Lifecycle      RouteLifecycle `json:"lifecycle"`
	CleanupFailure string         `json:"cleanupFailure,omitempty"`
}

// StoredRouteBindings validates every binding and its matching finalizer.
func StoredRouteBindings(config Config, route *gatewayv1.HTTPRoute) ([]RouteBinding, error) {
	var bindings []RouteBinding
	if encoded := route.Annotations[config.RouteBindingsAnnotation()]; encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &bindings); err != nil {
			return nil, fmt.Errorf("%w: HTTPRoute binding list is invalid", cloud.ErrOwnershipConflict)
		}
	}
	if len(bindings) == 0 {
		if controllerutil.ContainsFinalizer(route, config.RouteFinalizer()) || hasRouteBindingFinalizer(config, route) {
			return nil, fmt.Errorf("%w: HTTPRoute has finalizers without a complete Gateway binding", cloud.ErrOwnershipConflict)
		}
		return nil, nil
	}
	if !controllerutil.ContainsFinalizer(route, config.RouteFinalizer()) {
		return nil, fmt.Errorf("%w: HTTPRoute binding has no controller finalizer", cloud.ErrOwnershipConflict)
	}
	finalizers := make(map[string]struct{}, len(bindings))
	seen := make(map[string]struct{}, len(bindings))
	for i := range bindings {
		binding := &bindings[i]
		id := binding.Identity
		if err := cloud.ValidateRouteIdentity(id); err != nil {
			return nil, fmt.Errorf("%w: HTTPRoute binding identity is incomplete", cloud.ErrOwnershipConflict)
		}
		if id.OpenStackProjectID == "" || id.OpenStackProjectID != config.OpenStackProjectID || id.ClusterID != config.ClusterID || id.Controller != string(config.ControllerName) ||
			id.RouteNamespace != route.Namespace || id.RouteName != route.Name || id.RouteUID != string(route.UID) {
			return nil, fmt.Errorf("%w: HTTPRoute binding identity differs from the controller or Route", cloud.ErrOwnershipConflict)
		}
		if binding.Lifecycle != RouteAttached && binding.Lifecycle != RouteDetaching {
			return nil, fmt.Errorf("%w: HTTPRoute binding lifecycle is missing or invalid", cloud.ErrOwnershipConflict)
		}
		if _, exists := seen[id.GatewayUID]; exists {
			return nil, fmt.Errorf("%w: HTTPRoute has duplicate Gateway bindings", cloud.ErrOwnershipConflict)
		}
		seen[id.GatewayUID] = struct{}{}
		finalizer := routeBindingFinalizer(config, id)
		if !controllerutil.ContainsFinalizer(route, finalizer) {
			return nil, fmt.Errorf("%w: HTTPRoute binding does not match its finalizer", cloud.ErrOwnershipConflict)
		}
		finalizers[finalizer] = struct{}{}
		// Release metadata is not immutable ownership.
		binding.Identity.ControllerVersion = config.ControllerVersion
	}
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, config.RouteBindingFinalizerPrefix()) {
			if _, expected := finalizers[finalizer]; !expected {
				return nil, fmt.Errorf("%w: HTTPRoute has an unmatched Gateway binding finalizer", cloud.ErrOwnershipConflict)
			}
		}
	}
	slices.SortFunc(bindings, func(a, b RouteBinding) int { return strings.Compare(a.Identity.GatewayUID, b.Identity.GatewayUID) })
	return bindings, nil
}

// StoredRouteBinding returns the validated binding for one Gateway UID.
func StoredRouteBinding(config Config, route *gatewayv1.HTTPRoute, gatewayUID string) (RouteBinding, bool, error) {
	bindings, err := StoredRouteBindings(config, route)
	if err != nil {
		return RouteBinding{}, false, err
	}
	for _, binding := range bindings {
		if binding.Identity.GatewayUID == gatewayUID {
			return binding, true, nil
		}
	}
	return RouteBinding{}, false, nil
}

// SetRouteBinding adds or replaces one entry without changing other bindings.
// Callers must validate the source snapshot before mutating it.
func SetRouteBinding(config Config, route *gatewayv1.HTTPRoute, binding RouteBinding) error {
	desired := route.DeepCopy()
	bindings, err := StoredRouteBindings(config, desired)
	if err != nil {
		return err
	}
	found := false
	for i := range bindings {
		if bindings[i].Identity.GatewayUID == binding.Identity.GatewayUID {
			if bindings[i].Identity != binding.Identity {
				return fmt.Errorf("%w: HTTPRoute binding identity cannot change", cloud.ErrOwnershipConflict)
			}
			bindings[i] = binding
			found = true
		}
	}
	if !found {
		bindings = append(bindings, binding)
	}
	if err := storeRouteBindings(config, desired, bindings); err != nil {
		return err
	}
	controllerutil.AddFinalizer(desired, config.RouteFinalizer())
	controllerutil.AddFinalizer(desired, routeBindingFinalizer(config, binding.Identity))
	if _, err := StoredRouteBindings(config, desired); err != nil {
		return err
	}
	*route = *desired
	return nil
}

// RemoveRouteBinding removes only the specified binding after confirmed cleanup.
func RemoveRouteBinding(config Config, route *gatewayv1.HTTPRoute, gatewayUID string) error {
	bindings, err := StoredRouteBindings(config, route)
	if err != nil {
		return err
	}
	remaining := make([]RouteBinding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Identity.GatewayUID == gatewayUID {
			controllerutil.RemoveFinalizer(route, routeBindingFinalizer(config, binding.Identity))
			continue
		}
		remaining = append(remaining, binding)
	}
	if len(remaining) == 0 {
		delete(route.Annotations, config.RouteBindingsAnnotation())
		controllerutil.RemoveFinalizer(route, config.RouteFinalizer())
		return nil
	}
	return storeRouteBindings(config, route, remaining)
}

func storeRouteBindings(config Config, route *gatewayv1.HTTPRoute, bindings []RouteBinding) error {
	slices.SortFunc(bindings, func(a, b RouteBinding) int { return strings.Compare(a.Identity.GatewayUID, b.Identity.GatewayUID) })
	encoded, err := json.Marshal(bindings)
	if err != nil {
		return fmt.Errorf("encode HTTPRoute bindings: %w", err)
	}
	if route.Annotations == nil {
		route.Annotations = make(map[string]string)
	}
	route.Annotations[config.RouteBindingsAnnotation()] = string(encoded)
	return nil
}

func routeBindingFinalizer(config Config, id cloud.Identity) string {
	return config.RouteBindingFinalizer(id.ClusterID, id.OpenStackProjectID, id.GatewayNamespace, id.GatewayName, id.GatewayUID)
}

func hasRouteBindingFinalizer(config Config, route *gatewayv1.HTTPRoute) bool {
	for _, finalizer := range route.Finalizers {
		if strings.HasPrefix(finalizer, config.RouteBindingFinalizerPrefix()) {
			return true
		}
	}
	return false
}

// RouteHasControllerBinding reports any durable cleanup responsibility.
func RouteHasControllerBinding(config Config, route *gatewayv1.HTTPRoute) bool {
	return controllerutil.ContainsFinalizer(route, config.RouteFinalizer()) || hasRouteBindingFinalizer(config, route) || route.Annotations[config.RouteBindingsAnnotation()] != ""
}
