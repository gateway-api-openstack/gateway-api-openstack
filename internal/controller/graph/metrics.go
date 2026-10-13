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

package graph

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Stage separates graph coordination, input reads, validation, and provider work.
type Stage string

const (
	// StageLockWait measures time waiting for the Gateway UID coordinator.
	StageLockWait Stage = "lock_wait"
	// StageGatewayInput includes the Gateway source's reads and model building.
	StageGatewayInput Stage = "gateway_input"
	// StageRouteInput includes route source reads and model building.
	StageRouteInput Stage = "route_input"
	// StageValidation measures the live mutation fences for collected inputs.
	StageValidation Stage = "validation"
	// StageProvider measures one complete provider operation, including local
	// rate limit waits. It does not measure asynchronous Octavia convergence.
	StageProvider Stage = "provider"
)

// StageObserver records request-local stage durations without resource identity.
type StageObserver interface {
	ObserveGraphStage(Stage, time.Duration, bool)
}

// WriterMetrics records stage latency using bounded stage and outcome labels.
type WriterMetrics struct {
	durations *prometheus.HistogramVec
}

// RegisterWriterMetrics registers metrics in the controller's existing registry.
// Repeated setup reuses a compatible collector without losing prior observations.
func RegisterWriterMetrics(registerer prometheus.Registerer) (*WriterMetrics, error) {
	if registerer == nil {
		return nil, errors.New("graph metrics registerer is required")
	}
	collector := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "gateway_api_openstack",
		Subsystem: "graph_writer",
		Name:      "stage_duration_seconds",
		Help:      "Gateway graph writer stage duration, including input reads and provider waits within each stage.",
		Buckets:   []float64{0.0001, 0.001, 0.01, 0.1, 1, 5, 15, 60, 120},
	}, []string{"stage", "outcome"})
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if !errors.As(err, &alreadyRegistered) {
			return nil, fmt.Errorf("register graph writer duration: %w", err)
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.HistogramVec)
		if !ok {
			return nil, fmt.Errorf("existing graph writer collector has type %T, want *prometheus.HistogramVec", alreadyRegistered.ExistingCollector)
		}
		collector = existing
	}
	return &WriterMetrics{durations: collector}, nil
}

// ObserveGraphStage observes only fixed stage names and success or error outcomes.
func (m *WriterMetrics) ObserveGraphStage(stage Stage, duration time.Duration, failed bool) {
	if m == nil {
		return
	}
	switch stage {
	case StageLockWait, StageGatewayInput, StageRouteInput, StageValidation, StageProvider:
	default:
		stage = "unknown"
	}
	outcome := "success"
	if failed {
		outcome = "error"
	}
	if duration < 0 {
		duration = 0
	}
	m.durations.WithLabelValues(string(stage), outcome).Observe(duration.Seconds())
}

var _ StageObserver = (*WriterMetrics)(nil)
