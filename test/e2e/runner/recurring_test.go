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

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/runconfig"
)

func recurringFile(t *testing.T) fileConfig {
	t.Helper()
	file := validFileConfig()
	file.RunID = ""
	file.OpenStack.ProjectMode = runconfig.ProjectModeDedicated
	file.OpenStack.DedicatedForE2E = true
	file.OpenStack.AcceptProjectWideCredentialRisk = false
	file.Kubernetes.ExpectedAPIServer = "https://cluster.example.test:6443"
	file.Kubernetes.ExpectedClusterUID = "cluster-uid"
	file.Backend.Replicas = 2
	file.Backend.NodeSelector = map[string]string{"example.test/e2e": "true"}
	file.Scenarios.MemberTransitions = true
	file.Artifacts.Root = t.TempDir()
	return file
}

func TestRecurringProfilesKeepScopeAndHaveSeparateIdentity(t *testing.T) {
	file := recurringFile(t)
	original := file
	configs, err := resolveRunConfigs(file, resolveOptions{repositoryRoot: testRepositoryRoot(t)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 || configs[0].RunID == configs[1].RunID || configs[0].Namespace == configs[1].Namespace {
		t.Fatal("recurring profiles do not have separate identities")
	}
	if configs[0].BackendExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster || configs[1].BackendExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Fatal("recurring profiles do not cover both traffic policies")
	}
	if !reflect.DeepEqual(configs[0].Project, configs[1].Project) || configs[0].ExpectedClusterUID != configs[1].ExpectedClusterUID ||
		configs[0].ControllerImage != configs[1].ControllerImage || !reflect.DeepEqual(file, original) {
		t.Fatal("recurring resolution changed the approved scope, artifact, or input")
	}
}

func TestRecurringRejectsIncompleteScope(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*fileConfig)
	}{
		{name: "shared project", change: func(file *fileConfig) { file.OpenStack.ProjectMode = runconfig.ProjectModeShared }},
		{name: "missing acknowledgement", change: func(file *fileConfig) { file.OpenStack.DedicatedForE2E = false }},
		{name: "fixed run ID", change: func(file *fileConfig) { file.RunID = "fixed-run" }},
		{name: "missing transitions", change: func(file *fileConfig) { file.Scenarios.MemberTransitions = false }},
		{name: "Local-only lifecycle", change: func(file *fileConfig) { file.Scenarios.LocalLifecycle = true }},
		{name: "missing cluster fence", change: func(file *fileConfig) { file.Kubernetes.ExpectedClusterUID = "" }},
		{name: "one backend", change: func(file *fileConfig) { file.Backend.Replicas = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := recurringFile(t)
			test.change(&file)
			if _, err := resolveRunConfigs(file, resolveOptions{repositoryRoot: testRepositoryRoot(t)}, true); err == nil {
				t.Fatal("accepted incomplete recurring scope")
			}
		})
	}
}

func TestRecurringArtifactsSurviveCheckoutCleanup(t *testing.T) {
	repository := t.TempDir()
	inside := filepath.Join(repository, "evidence")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(t.TempDir(), "evidence")
	if err := os.Symlink(inside, symlink); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"", "relative", repository, inside, symlink} {
		if err := validateRecurringArtifacts(directory, repository); err == nil {
			t.Fatalf("accepted disposable artifact path %q", directory)
		}
	}
	if err := validateRecurringArtifacts(t.TempDir(), repository); err != nil {
		t.Fatal(err)
	}
}

func TestSequentialRunsStopAfterFailureOrCancellation(t *testing.T) {
	configs := []resolvedConfig{{RunID: "first"}, {RunID: "second"}}
	failure := errors.New("cleanup incomplete")
	calls := 0
	err := runSequentially(t.Context(), configs, runOptions{}, func(context.Context, resolvedConfig, runOptions) error {
		calls++
		return failure
	})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("continued after cleanup failure: calls=%d, error=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	calls = 0
	err = runSequentially(ctx, configs, runOptions{}, func(context.Context, resolvedConfig, runOptions) error {
		calls++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("continued after cancellation: calls=%d, error=%v", calls, err)
	}
}
