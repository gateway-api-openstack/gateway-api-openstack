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

package diagnostic

import (
	"encoding/json"
	"fmt"
	"os"
)

type report struct {
	FormatVersion    string   `json:"formatVersion"`
	Kind             string   `json:"kind"`
	Test             string   `json:"test"`
	UpstreamVersion  string   `json:"upstreamVersion"`
	UpstreamRevision string   `json:"upstreamRevision"`
	Status           string   `json:"status"`
	Cleanup          string   `json:"cleanup"`
	Reason           string   `json:"reason"`
	Adaptations      []string `json:"adaptations"`
}

func newReport() report {
	return report{
		FormatVersion: "v1alpha1", Kind: "AdaptedUpstreamDiagnostic", Test: selectedTest,
		UpstreamVersion: upstreamVersion, UpstreamRevision: upstreamRevision,
		Status: "notrun", Cleanup: "notneeded", Reason: "No test has run",
		Adaptations: []string{
			"Only HTTPRouteSimpleSameNamespace assertions run; this is not a conformance profile report",
			"Default suite setup is replaced by one HTTP Gateway and one backend in a new namespace",
			"Backend Service uses NodePort and Local traffic policy, with one selected worker Pod",
			"The operator supplies a digest-pinned echo-basic image and an existing accepted class configuration",
			"Runtime supportedFeatures is unchanged; diagnostic feature selection is local to the harness",
			"Cleanup observes Kubernetes deletion; no independent OpenStack leak audit runs",
		},
	}
}

func writeReport(file *os.File, value report) error {
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek diagnostic report: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate diagnostic report: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write diagnostic report: %w", err)
	}
	return file.Sync()
}
