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

package model

import (
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// BackendAuthorization records whether the source may reference the target.
type BackendAuthorization string

const (
	// BackendAuthorizationNotChecked applies to unsupported reference kinds.
	BackendAuthorizationNotChecked BackendAuthorization = "NotChecked"
	// BackendAuthorizationAllowed permits resolving the referenced Service.
	BackendAuthorizationAllowed BackendAuthorization = "Allowed"
	// BackendAuthorizationDenied prevents resolving a cross-namespace target.
	BackendAuthorizationDenied BackendAuthorization = "Denied"
)

// BackendState distinguishes reference validity from endpoint availability.
type BackendState string

const (
	// BackendResolved identifies an authorized Service port, even with no endpoints.
	BackendResolved BackendState = "Resolved"
	// BackendInvalid retains a backend's weight and the reason it cannot resolve.
	BackendInvalid BackendState = "Invalid"
)

// BackendGroup preserves one backendRef's contribution independently of endpoints.
// Groups are not combined even when their Services or endpoints overlap.
type BackendGroup struct {
	Reference     gatewayv1.HTTPBackendRef
	Weight        int32
	Authorization BackendAuthorization
	State         BackendState
	Service       types.NamespacedName
	ServiceUID    types.UID
	Port          *corev1.ServicePort
	Endpoints     []Endpoint
	Diagnostic    *Diagnostic
}

// Endpoint is a resolved address and port with the observed Kubernetes conditions.
// Conditions have their Kubernetes defaults applied. Different observations of
// the same address and port remain distinct until a member mode defines selection
// and draining. This model does not infer Amphora health or network reachability.
type Endpoint struct {
	Address     string
	Port        int32
	AddressType discoveryv1.AddressType
	Ready       bool
	Serving     bool
	Terminating bool
}
