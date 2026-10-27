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

// Package cloud defines the provider-neutral boundary used by the Kubernetes
// reconcilers. Provider SDK types must not cross this package boundary.
package cloud

import (
	"fmt"
	"slices"
)

// Identity is the immutable ownership anchor for a Gateway and its resources.
// Route fields are set only for resources whose lifecycle is owned by one
// HTTPRoute.
type Identity struct {
	OpenStackProjectID string `json:"openStackProjectID,omitempty"`
	ClusterID          string `json:"clusterID"`
	Controller         string `json:"controller"`
	ControllerVersion  string `json:"controllerVersion,omitempty"`
	GatewayNamespace   string `json:"gatewayNamespace"`
	GatewayName        string `json:"gatewayName"`
	GatewayUID         string `json:"gatewayUID"`
	RouteNamespace     string `json:"routeNamespace,omitempty"`
	RouteName          string `json:"routeName,omitempty"`
	RouteUID           string `json:"routeUID,omitempty"`
}

// GatewaySpec describes the OpenStack resources owned by a Gateway.
type GatewaySpec struct {
	Identity            Identity
	Provider            string
	VIPSubnetID         string
	ExternalNetworkID   string
	FlavorID            string
	AvailabilityZone    string
	ListenerName        string
	ListenerPort        int
	VIPSecurityGroupIDs []string
}

// GatewayState is the provider-neutral result published to Gateway status and
// passed to route reconciliation.
type GatewayState struct {
	LoadBalancerID    string
	VIPPortID         string
	VIPAddress        string
	FloatingIPID      string
	FloatingIPAddress string
	ListenerID        string
}

// GatewayRequirements contains the immutable Gateway fields and the settings
// that route reconciliation must verify before it can restore traffic.
type GatewayRequirements struct {
	Provider            string
	VIPSubnetID         string
	ExternalNetworkID   string
	FlavorID            string
	AvailabilityZone    string
	ListenerPort        int
	VIPSecurityGroupIDs []string
}

// Equal compares the frozen requirements, including canonical security group IDs.
func (r GatewayRequirements) Equal(other GatewayRequirements) bool {
	return r.Provider == other.Provider && r.VIPSubnetID == other.VIPSubnetID &&
		r.ExternalNetworkID == other.ExternalNetworkID && r.FlavorID == other.FlavorID &&
		r.AvailabilityZone == other.AvailabilityZone && r.ListenerPort == other.ListenerPort &&
		slices.Equal(r.VIPSecurityGroupIDs, other.VIPSecurityGroupIDs)
}

// Address returns the client-facing address, preferring a Floating IP when
// one is configured.
func (s GatewayState) Address() string {
	if s.FloatingIPAddress != "" {
		return s.FloatingIPAddress
	}
	return s.VIPAddress
}

// PathMatchType is the supported HTTP path matching subset.
type PathMatchType string

const (
	// PathMatchExact maps to an Octavia EQUAL_TO path rule.
	PathMatchExact PathMatchType = "Exact"
	// PathMatchPrefix maps to an exact policy plus a segment-prefix policy so
	// /foo does not accidentally match /foobar.
	PathMatchPrefix PathMatchType = "PathPrefix"
)

// Member is one Octavia backend member.
type Member struct {
	Address string
	Port    int
	// ProviderID identifies the Node's server for read-only worker-port validation.
	// It is not part of the Octavia member identity, which remains address and port.
	ProviderID string
}

// RouteSpec describes the pool and L7 policies owned by one route.
type RouteSpec struct {
	Identity                Identity
	Gateway                 GatewayState
	GatewayRequirements     GatewayRequirements
	MemberSubnetID          string
	BackendSecurityGroupIDs []string
	HealthMonitor           HealthMonitorConfig
	Hostname                string
	PathType                PathMatchType
	PathValue               string
	Members                 []Member
}

// RouteState records the OpenStack objects produced for one HTTPRoute.
type RouteState struct {
	PoolID      string
	MemberIDs   []string
	MonitorID   string
	L7PolicyIDs []string
	L7RuleIDs   []string
}

// ValidateGatewayIdentity verifies the common Gateway ownership tuple.
func ValidateGatewayIdentity(id Identity) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "cluster ID", value: id.ClusterID},
		{name: "controller", value: id.Controller},
		{name: "Gateway namespace", value: id.GatewayNamespace},
		{name: "Gateway name", value: id.GatewayName},
		{name: "Gateway UID", value: id.GatewayUID},
	} {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	return nil
}

// ValidateRouteIdentity verifies both the Gateway and HTTPRoute ownership
// tuples.
func ValidateRouteIdentity(id Identity) error {
	if err := ValidateGatewayIdentity(id); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "HTTPRoute namespace", value: id.RouteNamespace},
		{name: "HTTPRoute name", value: id.RouteName},
		{name: "HTTPRoute UID", value: id.RouteUID},
	} {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	return nil
}
