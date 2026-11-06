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
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
)

const maximumNetworkRequestEvidence = 4096

type networkStage string

const (
	networkDeleteFloatingIP   networkStage = "delete owned Floating IP and confirm absence"
	networkObserveReplacement networkStage = "observe replacement Floating IP"
	networkRecoverWorkload    networkStage = "verify Gateway and HTTP recovery"
	networkEmergencyCleanup   networkStage = "emergency cleanup"
)

type networkStageEvidence struct {
	Stage       networkStage `json:"stage"`
	StartedAt   time.Time    `json:"startedAt"`
	CompletedAt time.Time    `json:"completedAt,omitempty"`
	Status      checkStatus  `json:"status"`
	FailureKind string       `json:"failureKind,omitempty"`
}

type networkRequestEvidence struct {
	CompletedAt     time.Time                  `json:"completedAt"`
	Service         clients.RequestService     `json:"service"`
	Method          clients.RequestMethod      `json:"method"`
	StatusClass     clients.RequestStatusClass `json:"statusClass"`
	DurationSeconds float64                    `json:"durationSeconds"`
}

// networkEvidence records bounded classifications supplied by the client
// observer. Request URLs, headers, bodies, resource identities, and raw errors
// never enter this report.
type networkEvidence struct {
	mu              sync.Mutex
	Stages          []networkStageEvidence   `json:"stages"`
	Requests        []networkRequestEvidence `json:"requests"`
	DroppedRequests int                      `json:"droppedRequests"`
}

func (e *networkEvidence) MarshalJSON() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	type evidence networkEvidence
	return json.Marshal((*evidence)(e))
}

func (e *networkEvidence) begin(stage networkStage) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Stages = append(e.Stages, networkStageEvidence{Stage: stage, StartedAt: time.Now().UTC(), Status: statusNotRun})
	return len(e.Stages) - 1
}

func (e *networkEvidence) complete(index int, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stage := &e.Stages[index]
	stage.CompletedAt = time.Now().UTC()
	stage.Status = statusPassed
	if err != nil {
		stage.Status = statusFailed
		stage.FailureKind = networkFailureKind(err)
	}
}

// Request completions already carry the elapsed transport time. The interface
// requires a start callback, but this report does not need a second event.
func (*networkEvidence) ObserveRequestStart(clients.RequestStartObservation) {}

func (e *networkEvidence) ObserveRequest(observation clients.RequestObservation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.Requests) == maximumNetworkRequestEvidence {
		e.DroppedRequests++
		return
	}
	e.Requests = append(e.Requests, networkRequestEvidence{
		CompletedAt: time.Now().UTC(), Service: observation.Service, Method: observation.Method,
		StatusClass: observation.StatusClass, DurationSeconds: observation.Duration.Seconds(),
	})
}

func networkFailureKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context-deadline"
	case errors.Is(err, context.Canceled):
		return "context-canceled"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "transport-timeout"
	}
	if errors.Is(err, cloud.ErrTimeout) {
		return "provider-timeout"
	}
	return "other"
}
