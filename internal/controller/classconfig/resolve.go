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

// Package classconfig resolves the typed parameters of a GatewayClass.
package classconfig

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

// Resolved is the complete configuration recorded when a Gateway is bound.
// Reconciliation and cleanup of a bound Gateway do not resolve mutable selectors again.
type Resolved struct {
	Topology             cloud.Topology            `json:"topology"`
	NodeAddressType      corev1.NodeAddressType    `json:"nodeAddressType"`
	HealthMonitor        cloud.HealthMonitorConfig `json:"healthMonitor"`
	ParametersName       string                    `json:"parametersName"`
	ParametersUID        types.UID                 `json:"parametersUID"`
	ParametersGeneration int64                     `json:"parametersGeneration"`
}

// InvalidParametersError describes an evaluated configuration that cannot be used.
// Its message is safe to publish as a GatewayClass condition.
type InvalidParametersError struct{ Message string }

func (e *InvalidParametersError) Error() string { return e.Message }

// Resolve reads and validates the current class parameters before performing
// read-only infrastructure resolution. It never falls back to deployment flags.
func Resolve(ctx context.Context, reader client.Reader, resolver cloud.TopologyResolver, class *gatewayv1.GatewayClass) (Resolved, error) {
	ref := class.Spec.ParametersRef
	if ref == nil {
		return Resolved{}, invalid("GatewayClass requires parametersRef")
	}
	if ref.Group != gatewayv1.Group(configv1alpha1.GroupName) || ref.Kind != gatewayv1.Kind(configv1alpha1.GatewayClassConfigKind) || ref.Namespace != nil || ref.Name == "" {
		return Resolved{}, invalid("GatewayClass parametersRef must name a cluster-scoped OpenStackGatewayClassConfig in " + configv1alpha1.GroupName)
	}
	parameters := &configv1alpha1.OpenStackGatewayClassConfig{}
	if err := reader.Get(ctx, types.NamespacedName{Name: ref.Name}, parameters); err != nil {
		if apierrors.IsNotFound(err) {
			return Resolved{}, invalid("GatewayClass parameter object was not found")
		}
		return Resolved{}, fmt.Errorf("read GatewayClass parameters: %w", err)
	}
	if !parameters.DeletionTimestamp.IsZero() {
		return Resolved{}, invalid("GatewayClass parameter object is being deleted")
	}
	request, addressType, monitor, err := Validate(parameters.Spec)
	if err != nil {
		return Resolved{}, err
	}
	if resolver == nil {
		return Resolved{}, fmt.Errorf("GatewayClass topology resolver is required")
	}
	topology, err := resolver.ResolveTopology(ctx, request)
	if err != nil {
		if errors.Is(err, cloud.ErrTerminalValidation) {
			return Resolved{}, invalid("GatewayClass topology or requested Amphora capability could not be validated")
		}
		return Resolved{}, &topologyResolutionError{cause: err}
	}
	return Resolved{
		Topology: topology, NodeAddressType: addressType, HealthMonitor: monitor,
		ParametersName: parameters.Name, ParametersUID: parameters.UID, ParametersGeneration: parameters.Generation,
	}, nil
}

func invalid(message string) error { return &InvalidParametersError{Message: message} }

// Provider error bodies can contain project details or credentials. Preserve
// the cause for classification without exposing it through controller logs.
type topologyResolutionError struct{ cause error }

func (e *topologyResolutionError) Error() string { return "could not resolve GatewayClass topology" }
func (e *topologyResolutionError) Unwrap() error { return e.cause }
