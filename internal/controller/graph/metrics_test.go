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
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type stageObserverFunc func(Stage, time.Duration, bool)

func (f stageObserverFunc) ObserveGraphStage(stage Stage, duration time.Duration, failed bool) {
	f(stage, duration, failed)
}

func TestWriterObservesStageBoundaries(t *testing.T) {
	w := writerFixture()
	var stages []Stage
	w.Observer = stageObserverFunc(func(stage Stage, duration time.Duration, failed bool) {
		stages = append(stages, stage)
		if duration < 0 || failed {
			t.Errorf("unexpected stage observation %s: %s, failed=%t", stage, duration, failed)
		}
	})
	if _, err := w.ReconcileGraph(context.Background(), writerRequest()); err != nil {
		t.Fatal(err)
	}
	if want := []Stage{StageLockWait, StageGatewayInput, StageRouteInput, StageValidation, StageProvider}; !slices.Equal(stages, want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	stages = nil
	w.Gateway = gatewaySourceFunc(func(context.Context, Request) (GatewayInput, error) { return GatewayInput{}, errors.New("read failed") })
	w.Observer = stageObserverFunc(func(stage Stage, _ time.Duration, failed bool) {
		stages = append(stages, stage)
		if failed != (stage == StageGatewayInput) {
			t.Errorf("stage %s failed=%t", stage, failed)
		}
	})
	if _, err := w.ReconcileGraph(context.Background(), writerRequest()); err == nil {
		t.Fatal("source failure was lost")
	}
	if want := []Stage{StageLockWait, StageGatewayInput}; !slices.Equal(stages, want) {
		t.Fatalf("failed reconciliation stages = %v, want %v", stages, want)
	}
}

func TestWriterMetricsUseBoundedLabelsAndReuseRegistry(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := RegisterWriterMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.ObserveGraphStage(StageLockWait, time.Second, false)
	reused, err := RegisterWriterMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	reused.ObserveGraphStage(StageLockWait, time.Second, false)
	reused.ObserveGraphStage(Stage("arbitrary-object-identifier"), -time.Second, true)
	families, err := registry.Gather()
	if err != nil || len(families) != 1 {
		t.Fatalf("metrics families = %d, error = %v", len(families), err)
	}
	if len(families[0].Metric) != 2 {
		t.Fatalf("series = %d, want two bounded label sets", len(families[0].Metric))
	}
	for _, metric := range families[0].Metric {
		labels := map[string]string{}
		for _, label := range metric.Label {
			labels[label.GetName()] = label.GetValue()
		}
		if len(labels) != 2 {
			t.Fatalf("unexpected labels: %v", labels)
		}
		switch labels["stage"] {
		case "lock_wait":
			if labels["outcome"] != "success" || metric.Histogram.GetSampleCount() != 2 || metric.Histogram.GetSampleSum() != 2 {
				t.Fatalf("prior observations were not retained: %v", metric)
			}
		case "unknown":
			if labels["outcome"] != "error" || metric.Histogram.GetSampleSum() != 0 {
				t.Fatalf("unbounded or negative observation: %v", metric)
			}
		default:
			t.Fatalf("unbounded stage label: %v", labels)
		}
	}
}
