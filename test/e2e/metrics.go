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
	"bufio"
	"bytes"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/controllercontract"
)

const (
	maximumMetricsResponseBytes = 2 << 20

	mutationCounterMetricName      = "gateway_api_openstack_openstack_mutations_total"
	mutationInFlightMetricName     = "gateway_api_openstack_openstack_mutations_in_flight"
	requestCounterMetricName       = "gateway_api_openstack_openstack_requests_total"
	requestStartsMetricName        = "gateway_api_openstack_openstack_request_starts_total"
	requestDurationCountMetricName = "gateway_api_openstack_openstack_request_duration_seconds_count"
	leaderElectionStatusMetricName = "leader_election_master_status"
	processStartTimeMetricName     = "process_start_time_seconds"
)

var allowedMetricFamilies = map[string]struct{}{
	mutationCounterMetricName:      {},
	mutationInFlightMetricName:     {},
	requestCounterMetricName:       {},
	requestStartsMetricName:        {},
	requestDurationCountMetricName: {},
	leaderElectionStatusMetricName: {},
	processStartTimeMetricName:     {},
}

type metricSample struct {
	labels map[string]string
	value  float64
}

type metricSnapshot struct {
	values  map[string]float64
	samples map[string][]metricSample
}

type membershipEvidence struct {
	DurationSeconds  float64 `json:"durationSeconds"`
	Requests         float64 `json:"requests"`
	ReadRequests     float64 `json:"readRequests"`
	MutationAttempts float64 `json:"mutationAttempts"`
	RequestLimit     float64 `json:"requestLimit"`
	MutationLimit    float64 `json:"mutationLimit"`
	TrafficSamples   int     `json:"trafficSamples"`
	TrafficFailures  int     `json:"trafficFailures"`
}

func membershipMetricEvidence(before, after metricSnapshot, leaderLease string, duration time.Duration, mutations int) (membershipEvidence, error) {
	evidence := membershipEvidence{DurationSeconds: duration.Seconds(), MutationLimit: float64(mutations)}
	if duration <= 0 || mutations < 0 {
		return evidence, fmt.Errorf("membership observation requires a positive duration and a nonnegative mutation budget")
	}
	if err := validateConvergedMetricSnapshots(before, after, leaderLease); err != nil {
		return evidence, err
	}
	if err := validateCounterSeriesDoNotDecrease(before, after, requestStartsMetricName); err != nil {
		return evidence, err
	}
	var err error
	evidence.Requests, err = metricCounterDelta(before, after, requestStartsMetricName)
	if err != nil {
		return evidence, err
	}
	evidence.ReadRequests, _, evidence.MutationAttempts, err = convergedMetricDeltas(before, after)
	if err != nil {
		return evidence, err
	}
	// Admission counts, unlike completion counts, cannot include requests that
	// began before the first scrape. The measured duration encloses both scrapes.
	evidence.RequestLimit = math.Ceil(duration.Seconds()*controllercontract.APIQPS) + controllercontract.APIBurst
	if evidence.ReadRequests <= 0 || evidence.Requests <= 0 {
		return evidence, fmt.Errorf("membership transition did not include a fresh OpenStack observation")
	}
	if evidence.Requests > evidence.RequestLimit {
		return evidence, fmt.Errorf("membership transition exceeded the OpenStack request budget")
	}
	if evidence.MutationAttempts != evidence.MutationLimit {
		return evidence, fmt.Errorf("membership transition did not use exactly the expected member mutations")
	}
	return evidence, nil
}

func readMetricSnapshot(reader io.Reader) (metricSnapshot, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, maximumMetricsResponseBytes+1))
	if err != nil {
		return metricSnapshot{}, fmt.Errorf("read metrics response: %w", err)
	}
	if len(contents) > maximumMetricsResponseBytes {
		return metricSnapshot{}, fmt.Errorf("metrics response exceeds the safe size limit")
	}
	snapshot, err := parseMetricSnapshot(bytes.NewReader(contents))
	if err != nil {
		return metricSnapshot{}, err
	}
	if err := validateRequiredMetricFamilies(snapshot); err != nil {
		return metricSnapshot{}, err
	}
	return snapshot, nil
}

func validateRequiredMetricFamilies(snapshot metricSnapshot) error {
	for _, name := range []string{
		mutationCounterMetricName,
		mutationInFlightMetricName,
		requestCounterMetricName,
		requestDurationCountMetricName,
		leaderElectionStatusMetricName,
		processStartTimeMetricName,
	} {
		if _, found := snapshot.values[name]; !found {
			return fmt.Errorf("metrics endpoint did not expose required family %s", name)
		}
	}
	return nil
}

func metricCounterDelta(before, after metricSnapshot, name string) (float64, error) {
	beforeValue, found := before.values[name]
	if !found {
		return 0, fmt.Errorf("before snapshot did not contain counter %s", name)
	}
	afterValue, found := after.values[name]
	if !found {
		return 0, fmt.Errorf("after snapshot did not contain counter %s", name)
	}
	if afterValue < beforeValue {
		return 0, fmt.Errorf("counter %s decreased during the observation window", name)
	}
	return afterValue - beforeValue, nil
}

func validateConvergedNoOp(before, after metricSnapshot, leaderLease string) error {
	if err := validateConvergedMetricSnapshots(before, after, leaderLease); err != nil {
		return err
	}
	requestDelta, durationDelta, mutationDelta, err := convergedMetricDeltas(before, after)
	if err != nil {
		return err
	}
	if requestDelta <= 0 || durationDelta <= 0 {
		return fmt.Errorf("the controller did not perform a fresh OpenStack observation")
	}
	if mutationDelta != 0 {
		return fmt.Errorf("the controller attempted an OpenStack mutation over converged input")
	}
	return nil
}

func validateConvergedMetricSnapshots(before, after metricSnapshot, leaderLease string) error {
	if err := validateRequiredMetricFamilies(before); err != nil {
		return err
	}
	if err := validateRequiredMetricFamilies(after); err != nil {
		return err
	}
	if err := validateMetricEndpointIdentity(before, leaderLease); err != nil {
		return fmt.Errorf("validate first metrics snapshot: %w", err)
	}
	if err := validateMetricEndpointIdentity(after, leaderLease); err != nil {
		return fmt.Errorf("validate second metrics snapshot: %w", err)
	}
	if before.values[processStartTimeMetricName] != after.values[processStartTimeMetricName] {
		return fmt.Errorf("metrics process changed during the observation window")
	}
	for _, name := range []string{requestCounterMetricName, requestDurationCountMetricName, mutationCounterMetricName} {
		if err := validateCounterSeriesDoNotDecrease(before, after, name); err != nil {
			return err
		}
	}
	return nil
}

func convergedMetricDeltas(before, after metricSnapshot) (float64, float64, float64, error) {
	requestDelta, err := boundedMetricDelta(before, after, requestCounterMetricName, map[string]string{
		"service": "octavia",
		"method":  http.MethodGet,
	})
	if err != nil {
		return 0, 0, 0, err
	}
	durationDelta, err := boundedMetricDelta(before, after, requestDurationCountMetricName, map[string]string{
		"service": "octavia",
		"method":  http.MethodGet,
	})
	if err != nil {
		return 0, 0, 0, err
	}
	mutationDelta, err := metricCounterDelta(before, after, mutationCounterMetricName)
	if err != nil {
		return 0, 0, 0, err
	}
	return requestDelta, durationDelta, mutationDelta, nil
}

func validateMetricEndpointIdentity(snapshot metricSnapshot, leaderLease string) error {
	leaderSamples := snapshot.samples[leaderElectionStatusMetricName]
	if len(leaderSamples) != 1 || len(leaderSamples[0].labels) != 1 || leaderSamples[0].labels["name"] != leaderLease ||
		leaderSamples[0].value != 1 {
		return fmt.Errorf("metrics endpoint did not expose exactly one current leader sample for the configured Lease")
	}
	processSamples := snapshot.samples[processStartTimeMetricName]
	if len(processSamples) != 1 || len(processSamples[0].labels) != 0 || processSamples[0].value <= 0 {
		return fmt.Errorf("metrics endpoint did not expose exactly one valid process start sample")
	}
	for _, sample := range snapshot.samples[mutationInFlightMetricName] {
		if sample.value != 0 {
			return fmt.Errorf("an OpenStack mutation was in flight at an observation boundary")
		}
	}
	return nil
}

func validateCounterSeriesDoNotDecrease(before, after metricSnapshot, name string) error {
	afterByLabels := make(map[string]float64, len(after.samples[name]))
	for _, sample := range after.samples[name] {
		afterByLabels[canonicalMetricLabels(sample.labels)] = sample.value
	}
	for _, sample := range before.samples[name] {
		afterValue, found := afterByLabels[canonicalMetricLabels(sample.labels)]
		if !found || afterValue < sample.value {
			return fmt.Errorf("counter %s decreased or lost a label set during the observation window", name)
		}
	}
	return nil
}

func boundedMetricDelta(before, after metricSnapshot, name string, labels map[string]string) (float64, error) {
	beforeValue := sumMetricSamples(before.samples[name], labels)
	afterValue := sumMetricSamples(after.samples[name], labels)
	if afterValue < beforeValue {
		return 0, fmt.Errorf("bounded counter %s decreased during the observation window", name)
	}
	return afterValue - beforeValue, nil
}

func sumMetricSamples(samples []metricSample, labels map[string]string) float64 {
	var total float64
	for _, sample := range samples {
		matches := true
		for name, expected := range labels {
			if sample.labels[name] != expected {
				matches = false
				break
			}
		}
		if matches {
			total += sample.value
		}
	}
	return total
}

func parseMetricSnapshot(reader io.Reader) (metricSnapshot, error) {
	// Keep only the sample lines we use. Omitting TYPE metadata lets the parser
	// retain histogram counts as individual samples, including duplicate series.
	var selected strings.Builder
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maximumMetricsResponseBytes+1)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		nameEnd := strings.IndexAny(line, "{ \t")
		if nameEnd < 0 {
			nameEnd = len(line)
		}
		if _, allowed := allowedMetricFamilies[line[:nameEnd]]; allowed {
			selected.WriteString(line)
			selected.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return metricSnapshot{}, fmt.Errorf("read metrics response: %w", err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(selected.String()))
	if err != nil {
		return metricSnapshot{}, fmt.Errorf("parse metrics response: %w", err)
	}
	snapshot := metricSnapshot{
		values:  make(map[string]float64),
		samples: make(map[string][]metricSample),
	}
	seen := make(map[string]map[string]struct{})
	for _, name := range slices.Sorted(maps.Keys(families)) {
		for _, metric := range families[name].Metric {
			sample := metricSample{labels: make(map[string]string), value: metric.GetUntyped().GetValue()}
			if math.IsNaN(sample.value) || math.IsInf(sample.value, 0) || sample.value < 0 {
				return metricSnapshot{}, fmt.Errorf("allowlisted metric %s requires a finite nonnegative value", name)
			}
			for _, label := range metric.Label {
				if _, duplicate := sample.labels[label.GetName()]; duplicate {
					return metricSnapshot{}, fmt.Errorf("allowlisted metric %s repeats a label name", name)
				}
				sample.labels[label.GetName()] = label.GetValue()
			}
			if err := recordMetricSample(&snapshot, seen, name, sample); err != nil {
				return metricSnapshot{}, err
			}
		}
	}
	return snapshot, nil
}

func recordMetricSample(snapshot *metricSnapshot, seen map[string]map[string]struct{}, name string, sample metricSample) error {
	labelKey := canonicalMetricLabels(sample.labels)
	if seen[name] == nil {
		seen[name] = make(map[string]struct{})
	}
	if _, duplicate := seen[name][labelKey]; duplicate {
		return fmt.Errorf("allowlisted metric %s repeats a label set", name)
	}
	seen[name][labelKey] = struct{}{}
	snapshot.values[name] += sample.value
	snapshot.samples[name] = append(snapshot.samples[name], sample)
	return nil
}

func canonicalMetricLabels(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	slices.Sort(names)
	var output strings.Builder
	for _, name := range names {
		output.WriteString(strconv.Quote(name))
		output.WriteByte('=')
		output.WriteString(strconv.Quote(labels[name]))
		output.WriteByte(',')
	}
	return output.String()
}
