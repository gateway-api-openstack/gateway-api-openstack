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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/runconfig"
)

func TestOverallStatusRequiresBaseline(t *testing.T) {
	report := newE2EReport(time.Unix(1, 0), reportTestConfig(projectModeDedicated))
	if got := overallStatus(report.Checks, report.requiredChecks...); got != statusNotRun {
		t.Fatalf("overallStatus() = %q, want %q", got, statusNotRun)
	}
	for _, name := range report.requiredChecks {
		if err := report.setCheck(name, statusPassed, checkSummaryPassed); err != nil {
			t.Fatal(err)
		}
	}
	if got := overallStatus(report.Checks, report.requiredChecks...); got != statusPassed {
		t.Fatalf("overallStatus() = %q, want %q", got, statusPassed)
	}
	for _, test := range []struct {
		name   string
		status checkStatus
		want   checkStatus
	}{
		{name: checkCleanup, status: statusNotRun, want: statusNotRun},
		{name: checkCleanup, status: statusSkipped, want: statusNotRun},
		{name: checkCleanup, status: statusFailed, want: statusFailed},
		{name: "quota failure", status: statusFailed, want: statusFailed},
	} {
		t.Run(test.name+"/"+string(test.status), func(t *testing.T) {
			checks := append([]checkResult(nil), report.Checks...)
			for i := range checks {
				if checks[i].Name == test.name {
					checks[i].Status = test.status
				}
			}
			if got := overallStatus(checks, report.requiredChecks...); got != test.want {
				t.Fatalf("overallStatus() = %q, want %q", got, test.want)
			}
		})
	}
	// A duplicate success cannot replace the required cleanup audit.
	checks := append([]checkResult(nil), report.Checks...)
	for i := range checks {
		if checks[i].Name == checkCleanupAudit {
			checks[i] = checks[0]
		}
	}
	if got := overallStatus(checks, report.requiredChecks...); got != statusNotRun {
		t.Fatalf("overallStatus() without cleanup audit = %q, want %q", got, statusNotRun)
	}
}

func TestSetCheckRejectsUnsafeSummaryShape(t *testing.T) {
	report := newE2EReport(time.Now(), reportTestConfig(projectModeDedicated))
	if err := report.setCheck(checkGateway, statusPassed, "line one\nline two"); err == nil {
		t.Fatal("setCheck() accepted a multiline summary")
	}
	if err := report.setCheck("missing check", statusPassed, checkSummaryPassed); err == nil || !strings.Contains(err.Error(), "unknown report check") {
		t.Fatal("setCheck() accepted an unknown check")
	}
	if err := report.setCheck(checkGateway, statusFailed, "secret-token"); err == nil {
		t.Fatal("setCheck() accepted a non-fixed summary")
	}
}

func TestLocalLifecycleReportRequiresEverySelectedScenario(t *testing.T) {
	for _, replicas := range []int32{1, 2} {
		config := reportTestConfig(projectModeShared)
		config.LocalLifecycle, config.BackendReplicas = true, replicas
		report := newE2EReport(time.Now(), config)
		for i := range report.Checks {
			if report.Checks[i].Status != statusSkipped {
				report.Checks[i].Status = statusPassed
			}
		}
		required := report.requiredChecks
		if got := overallStatus(report.Checks, required...); got != statusPassed {
			t.Fatalf("completed %d-worker report = %s", replicas, got)
		}
		for _, name := range required {
			for _, status := range []checkStatus{statusNotRun, statusSkipped, statusFailed} {
				checks := append([]checkResult(nil), report.Checks...)
				for i := range checks {
					if checks[i].Name == name {
						checks[i].Status = status
					}
				}
				want := statusNotRun
				if status == statusFailed {
					want = statusFailed
				}
				if got := overallStatus(checks, required...); got != want {
					t.Fatalf("%s %s: overall = %s, want %s", name, status, got, want)
				}
			}
			var missing []checkResult
			for _, check := range report.Checks {
				if check.Name != name {
					missing = append(missing, check)
				}
			}
			if got := overallStatus(missing, required...); got != statusNotRun {
				t.Fatalf("missing %s: overall = %s", name, got)
			}
		}
		if replicas == 1 {
			for _, check := range report.Checks {
				if check.Name == checkMemberTransitions && check.Status != statusSkipped {
					t.Fatal("single-worker report claimed member transition coverage")
				}
			}
		}
	}
}

func TestNetworkReportRequiresEverySelectedCheck(t *testing.T) {
	config := reportTestConfig(projectModeDedicated)
	config.FloatingIPRecovery = true
	config.FloatingIPDeletion = true
	config.Project.FrontendSecurityGroupIDs = []string{"private-group-id"}
	report := newE2EReport(time.Now(), config)
	for index := range report.Checks {
		report.Checks[index].Status = statusPassed
	}
	for _, name := range []string{checkFloatingIPRecovery, checkFloatingIPDeletion, checkReferencedSecurity} {
		if err := report.setCheck(name, statusNotRun, checkSummaryFailed); err != nil {
			t.Fatal(err)
		}
		if got := overallStatus(report.Checks, report.requiredChecks...); got != statusNotRun {
			t.Fatalf("unexecuted %s report = %s", name, got)
		}
		if err := report.setCheck(name, statusPassed, checkSummaryPassed); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(string(renderMarkdownReport(report)), "private-group-id") {
		t.Fatal("network evidence disclosed a configured security group ID")
	}
}

func TestWriteE2EArtifactsIsExclusiveAndSanitized(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "run-1234")
	config := reportTestConfig(projectModeShared)
	config.BackendExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	config.BackendNodeSelector = map[string]string{"private.example.test/node-pool": "private-node-pool"}
	report := newE2EReport(time.Unix(1, 0), config)
	if report.Backend.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal || !report.Backend.NodeSelectorConfigured {
		t.Fatalf("report backend profile = %#v", report.Backend)
	}
	report.CompletedAt = time.Unix(2, 0)
	if err := report.setCheck(checkPreflight, statusPassed, checkSummaryPassed); err != nil {
		t.Fatal(err)
	}
	if err := writeE2EArtifacts(directory, report); err != nil {
		t.Fatalf("writeE2EArtifacts() error = %v", err)
	}
	for _, name := range []string{"report.json", "report.md"} {
		assertE2EArtifactContents(t, filepath.Join(directory, name), config.ControllerRevision, config.ControllerImageDigest)
	}
	if err := writeE2EArtifacts(directory, report); err == nil {
		t.Fatal("writeE2EArtifacts() overwrote an existing artifact directory")
	}
}

func assertE2EArtifactContents(t *testing.T, path, revision, digest string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, forbidden := range []string{"secret-token", "project-id", "expected-vip-subnet", "expected-member-subnet", "192.0.2.10", "pod-uid", "private.example.test/node-pool", "private-node-pool"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("%s contains forbidden value %q", filepath.Base(path), forbidden)
		}
	}
	if !strings.Contains(text, revision) || !strings.Contains(text, digest) || !strings.Contains(text, string(projectModeShared)) {
		t.Fatalf("%s does not contain immutable controller evidence", filepath.Base(path))
	}
	if !strings.Contains(text, string(corev1.ServiceExternalTrafficPolicyLocal)) {
		t.Fatalf("%s does not record the Local backend profile", filepath.Base(path))
	}
}

func reportTestConfig(mode projectMode) e2eConfig {
	return e2eConfig{
		Project:                      runconfig.Project{Mode: mode},
		RestartMode:                  "cold",
		BackendReplicas:              2,
		ControllerRevision:           strings.Repeat("b", 40),
		ControllerImageDigest:        "sha256:" + strings.Repeat("a", 64),
		BackendExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
	}
}

func TestIndependentMemberTransitionCheckIsRequiredOnce(t *testing.T) {
	for _, lifecycle := range []bool{false, true} {
		config := reportTestConfig(projectModeDedicated)
		config.LocalLifecycle, config.MemberTransitions, config.BackendReplicas = lifecycle, true, 2
		report := newE2EReport(time.Now(), config)
		count := 0
		for i := range report.Checks {
			if report.Checks[i].Name == checkMemberTransitions {
				count++
				continue
			}
			report.Checks[i].Status = statusPassed
		}
		if count != 1 {
			t.Fatalf("member check count = %d", count)
		}
		path := filepath.Join(t.TempDir(), "report")
		if err := writeE2EArtifacts(path, report); err != nil {
			t.Fatal(err)
		}
		if report.Status != statusNotRun {
			t.Fatalf("unexecuted member scenario passed: %s", report.Status)
		}
	}
}

func TestConfiguredChecksSelectScenariosInExecutionOrder(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*e2eConfig)
		selected  []string
	}{
		{name: "baseline"},
		{name: "single-worker lifecycle", configure: func(c *e2eConfig) { c.LocalLifecycle = true; c.BackendReplicas = 1 }, selected: []string{checkFrozenClass, checkFrozenRestart, checkSequentialClasses}},
		{name: "two-worker lifecycle", configure: func(c *e2eConfig) { c.LocalLifecycle = true }, selected: []string{checkFrozenClass, checkFrozenRestart, checkMemberTransitions, checkSequentialClasses}},
		{name: "independent members", configure: func(c *e2eConfig) { c.MemberTransitions = true }, selected: []string{checkMemberTransitions}},
		{name: "network", configure: func(c *e2eConfig) {
			c.FloatingIPRecovery, c.FloatingIPDeletion = true, true
			c.Project.BackendSecurityGroupIDs = []string{"private-group"}
		}, selected: []string{checkReferencedSecurity, checkFloatingIPRecovery, checkFloatingIPDeletion}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := reportTestConfig(projectModeDedicated)
			if test.configure != nil {
				test.configure(&config)
			}
			checks := configuredChecks(config)
			seen := make(map[string]bool)
			var selected []string
			for _, name := range checks {
				if seen[name] {
					t.Fatalf("check %q was selected more than once", name)
				}
				seen[name] = true
				switch name {
				case checkReferencedSecurity, checkFloatingIPRecovery, checkFloatingIPDeletion,
					checkFrozenClass, checkFrozenRestart, checkMemberTransitions, checkSequentialClasses:
					selected = append(selected, name)
				}
			}
			if !slices.Equal(selected, test.selected) {
				t.Fatalf("selected checks = %v, want %v", selected, test.selected)
			}
			if checks[0] != checkPreflight || checks[len(checks)-2] != checkCleanup || checks[len(checks)-1] != checkCleanupAudit {
				t.Fatalf("checks lost preflight or final cleanup ordering: %v", checks)
			}
		})
	}
}
