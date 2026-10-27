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

import "context"

// ResourceReference selects one existing resource visible to the project.
// Exactly one of ID, Name, or Tags must be set.
type ResourceReference struct {
	ID   string
	Name string
	Tags []string
}

// TopologyRequest contains only existing infrastructure references.
// Resolution never creates, adopts, or modifies the selected resources.
type TopologyRequest struct {
	VIPSubnet             ResourceReference
	MemberSubnet          ResourceReference
	ExternalNetwork       *ResourceReference
	FlavorID              string
	AvailabilityZone      string
	VIPSecurityGroups     []ResourceReference
	BackendSecurityGroups []ResourceReference
}

// Topology is the validated infrastructure placement for a Gateway.
type Topology struct {
	VIPSubnetID             string   `json:"vipSubnetID"`
	MemberSubnetID          string   `json:"memberSubnetID"`
	ExternalNetworkID       string   `json:"externalNetworkID,omitempty"`
	FlavorID                string   `json:"flavorID,omitempty"`
	AvailabilityZone        string   `json:"availabilityZone,omitempty"`
	VIPSecurityGroupIDs     []string `json:"vipSecurityGroupIDs,omitempty"`
	BackendSecurityGroupIDs []string `json:"backendSecurityGroupIDs,omitempty"`
}

// TopologyResolver resolves project-visible references and verifies available
// Amphora capabilities without mutating cloud resources.
type TopologyResolver interface {
	ResolveTopology(context.Context, TopologyRequest) (Topology, error)
}

// HealthMonitorConfig describes the HTTP health checks for a route pool.
type HealthMonitorConfig struct {
	Path           string `json:"path"`
	Delay          int    `json:"delay"`
	Timeout        int    `json:"timeout"`
	MaxRetries     int    `json:"maxRetries"`
	MaxRetriesDown int    `json:"maxRetriesDown"`
}
