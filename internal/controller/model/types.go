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

// Package model compiles observed Kubernetes inputs into logical Gateway
// configuration. It neither grants cloud mutation authority nor selects an
// Octavia resource mapping.
package model

import (
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Input is an immutable, request-local observation supplied by the caller.
// The compiler does not establish its completeness or authorize references by
// virtue of their presence in these collections.
type Input struct {
	Gateway         *gatewayv1.Gateway
	Routes          []gatewayv1.HTTPRoute
	Namespaces      []corev1.Namespace
	Services        []corev1.Service
	EndpointSlices  []discoveryv1.EndpointSlice
	ReferenceGrants []gatewayv1.ReferenceGrant
}

// Source identifies the exact Kubernetes revision that contributed a model.
type Source struct {
	Namespace         string
	Name              string
	UID               types.UID
	Generation        int64
	ResourceVersion   string
	CreationTimestamp metav1.Time
}

// DiagnosticScope distinguishes an invalid rule from an invalid entire Route.
type DiagnosticScope string

const (
	// ScopeRule limits a diagnostic to the containing rule.
	ScopeRule DiagnosticScope = "Rule"
	// ScopeRoute rejects every rule of the containing Route.
	ScopeRoute DiagnosticScope = "Route"
)

// Diagnostic describes a logical input problem, not a persisted condition.
// Callers must apply their supported profile and status ownership rules.
type Diagnostic struct {
	Condition string
	Reason    string
	Message   string
	Scope     DiagnosticScope
}

func (d *Diagnostic) Error() string { return d.Message }

// Gateway contains the complete logical model for the supplied Gateway inputs.
// Diagnostics and unresolved actions must be checked before any cloud lowering.
type Gateway struct {
	Source      Source
	Listeners   []Listener
	Routes      []Route
	Diagnostics []Diagnostic
}

// Listener keeps its logical identity even if a future mapping shares resources.
type Listener struct {
	Name           gatewayv1.SectionName
	Port           gatewayv1.PortNumber
	Protocol       gatewayv1.ProtocolType
	Hostname       string
	AttachedRoutes int32
	Rules          []RouteMatch
	Diagnostics    []Diagnostic
}

// Route retains every rule and each ParentRef targeting this Gateway.
type Route struct {
	Source      Source
	Hostnames   []string
	Rules       []Rule
	Parents     []ParentAttachment
	Diagnostics []Diagnostic
}

// Rule retains its original position and Service groups independently of endpoints.
type Rule struct {
	Index       int
	Matches     []Match
	Backends    []BackendGroup
	Diagnostics []Diagnostic
}

// Match is one alternative within a rule. Its header predicates are conjunctive.
type Match struct {
	Index     int
	PathType  gatewayv1.PathMatchType
	PathValue string
	Headers   []HeaderMatch
}

// HeaderMatch is a normalized, case-insensitive name with an exact value.
type HeaderMatch struct {
	Name  string
	Value string
}

// AttachmentState is the outcome of logical attachment evaluation.
type AttachmentState string

const (
	// AttachmentAccepted indicates an allowed listener and hostname intersection.
	AttachmentAccepted AttachmentState = "Accepted"
	// AttachmentRejected indicates the parent or listener did not accept the Route.
	AttachmentRejected AttachmentState = "Rejected"
)

// ParentAttachment preserves an individual ParentRef and its selected listeners.
type ParentAttachment struct {
	Index       int
	Reference   gatewayv1.ParentReference
	State       AttachmentState
	Listeners   []ListenerAttachment
	Diagnostics []Diagnostic
}

// ListenerAttachment records the hostname intersection for one logical listener.
type ListenerAttachment struct {
	Name        gatewayv1.SectionName
	State       AttachmentState
	Hostnames   []string
	Diagnostics []Diagnostic
}

// RouteMatch identifies an ordered match without assigning a cloud policy ID.
// The referenced rule's diagnostics remain authoritative: this ordering is not
// a deployable graph and must not bypass unsupported actions or references.
type RouteMatch struct {
	Route      Source
	RuleIndex  int
	MatchIndex int
	Hostname   string
	Match      Match
}

func source(object metav1.Object) Source {
	return Source{
		Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(),
		Generation: object.GetGeneration(), ResourceVersion: object.GetResourceVersion(), CreationTimestamp: object.GetCreationTimestamp(),
	}
}
