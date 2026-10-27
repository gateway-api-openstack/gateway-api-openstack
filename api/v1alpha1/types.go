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

// Package v1alpha1 contains the OpenStack GatewayClass configuration API.
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// GroupName is the API group for project-owned configuration.
	GroupName = "gateway-api-openstack.github.io"
	// GatewayClassConfigKind is the cluster-scoped class parameter kind.
	GatewayClassConfigKind = "OpenStackGatewayClassConfig"
)

// FloatingIPMode determines which address the controller publishes.
type FloatingIPMode string

const (
	// FloatingIPNone publishes the load balancer VIP without allocating an address.
	FloatingIPNone FloatingIPMode = "None"
	// FloatingIPAllocate allocates a new Floating IP on the selected external network.
	FloatingIPAllocate FloatingIPMode = "Allocate"
)

// ResourceReference selects exactly one existing Neutron resource.
// Tags are an AND selector and must match exactly one visible resource.
type ResourceReference struct {
	ID   string   `json:"id,omitempty"`
	Name string   `json:"name,omitempty"`
	Tags []string `json:"tags,omitempty"`
}

// FloatingIPConfig controls allocation of an address owned by the Gateway.
type FloatingIPConfig struct {
	Mode            FloatingIPMode     `json:"mode,omitempty"`
	ExternalNetwork *ResourceReference `json:"externalNetwork,omitempty"`
}

// FrontendSecurityMode determines who configures access to the listener ports.
type FrontendSecurityMode string

const (
	// FrontendOctaviaManaged lets Octavia maintain listener security groups.
	FrontendOctaviaManaged FrontendSecurityMode = "OctaviaManaged"
	// FrontendReferenced uses existing groups without changing their rules.
	FrontendReferenced FrontendSecurityMode = "Referenced"
)

// FrontendSecurityConfig selects the existing groups supplied to Octavia.
// Referenced requires Octavia API 2.29 or newer and operator-managed ingress rules.
type FrontendSecurityConfig struct {
	Mode FrontendSecurityMode `json:"mode,omitempty"`
	// Groups selects 1 to 16 existing groups when Mode is Referenced.
	// +listType=atomic
	Groups []ResourceReference `json:"groups,omitempty"`
}

// BackendSecurityMode determines how backend security prerequisites are checked.
type BackendSecurityMode string

const (
	// BackendUnmanaged leaves backend security configuration to the operator.
	BackendUnmanaged BackendSecurityMode = "Unmanaged"
	// BackendReferenced verifies existing groups without changing groups or ports.
	BackendReferenced BackendSecurityMode = "Referenced"
)

// BackendSecurityConfig selects groups that must already protect the worker ports.
// Structural validation does not establish connectivity from Amphora to members.
type BackendSecurityConfig struct {
	Mode BackendSecurityMode `json:"mode,omitempty"`
	// Groups selects 1 to 16 existing groups when Mode is Referenced.
	// +listType=atomic
	Groups []ResourceReference `json:"groups,omitempty"`
}

// HealthMonitorConfig configures HTTP checks for each backend pool.
// Intervals use seconds. The method is GET and successful codes are 200-399.
type HealthMonitorConfig struct {
	Path           string `json:"path,omitempty"`
	Delay          *int32 `json:"delay,omitempty"`
	Timeout        *int32 `json:"timeout,omitempty"`
	MaxRetries     *int32 `json:"maxRetries,omitempty"`
	MaxRetriesDown *int32 `json:"maxRetriesDown,omitempty"`
}

// OpenStackGatewayClassConfigSpec supplies infrastructure defaults for new
// Gateways. Existing Gateways retain the complete validated configuration.
type OpenStackGatewayClassConfigSpec struct {
	VIPSubnet        ResourceReference      `json:"vipSubnet"`
	MemberSubnet     ResourceReference      `json:"memberSubnet"`
	FloatingIP       FloatingIPConfig       `json:"floatingIP,omitempty"`
	FrontendSecurity FrontendSecurityConfig `json:"frontendSecurity,omitempty"`
	BackendSecurity  BackendSecurityConfig  `json:"backendSecurity,omitempty"`
	// NodeAddressType is InternalIP or ExternalIP. It defaults to InternalIP.
	NodeAddressType string `json:"nodeAddressType,omitempty"`
	// FlavorID selects an enabled Amphora flavor by UUID.
	FlavorID string `json:"flavorID,omitempty"`
	// AvailabilityZone selects an enabled Amphora availability zone by exact name.
	AvailabilityZone string              `json:"availabilityZone,omitempty"`
	HealthMonitor    HealthMonitorConfig `json:"healthMonitor,omitempty"`
}

// OpenStackGatewayClassConfig supplies the parameters for a GatewayClass.
// Validation results are published on the referencing GatewayClass.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type OpenStackGatewayClassConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OpenStackGatewayClassConfigSpec `json:"spec"`
}

// OpenStackGatewayClassConfigList contains class configuration objects.
// +kubebuilder:object:root=true
type OpenStackGatewayClassConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenStackGatewayClassConfig `json:"items"`
}
