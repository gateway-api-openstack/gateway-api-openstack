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

// Package diagnostics describes read-only environment checks without exposing
// Kubernetes objects, provider SDK types, or private infrastructure identifiers.
package diagnostics

import (
	"context"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

// Node contains the inputs needed to verify a worker's Neutron port.
// Inputs are deliberately separate from the serializable report.
type Node struct {
	ProviderID string
	Address    string
}

// Request selects the existing infrastructure to inspect.
type Request struct {
	Topology      cloud.TopologyRequest
	Nodes         []Node
	CheckBarbican bool
}

// Status distinguishes demonstrated prerequisites from checks that remain unknown.
type Status string

const (
	Passed  Status = "passed"
	Failed  Status = "failed"
	Unknown Status = "unknown"
	Skipped Status = "skipped"
)

// Check records a fixed diagnostic message. Node is a one-based input ordinal,
// not a Kubernetes name or an OpenStack identifier.
type Check struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Node    int    `json:"node,omitempty"`
}

// Report is an experimental redacted summary, not provisioning authorization.
type Report struct {
	FormatVersion string    `json:"formatVersion"`
	ToolVersion   string    `json:"toolVersion,omitempty"`
	GeneratedAt   time.Time `json:"generatedAt,omitzero"`
	Checks        []Check   `json:"checks"`
	IPFamilies    []string  `json:"ipFamilies"`
}

// NewReport initializes JSON collections as arrays, including an empty result.
func NewReport() Report {
	return Report{FormatVersion: "v1alpha1", Checks: []Check{}, IPFamilies: []string{}}
}

// HasFailures reports observed failures, without treating an unproved check as success.
func (r Report) HasFailures() bool {
	for _, check := range r.Checks {
		if check.Status == Failed {
			return true
		}
	}
	return false
}

// Inspector performs only observation and returns independent check outcomes.
type Inspector interface {
	Inspect(context.Context, Request) Report
}
