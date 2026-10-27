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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/runconfig"
)

func resolveRunConfigs(file fileConfig, options resolveOptions, recurring bool) ([]resolvedConfig, error) {
	if !recurring {
		config, err := resolveFileConfig(file, options)
		if err != nil {
			return nil, err
		}
		return []resolvedConfig{config}, nil
	}
	if file.OpenStack.ProjectMode != runconfig.ProjectModeDedicated || !file.OpenStack.DedicatedForE2E {
		return nil, fmt.Errorf("recurring E2E requires a dedicated OpenStack project")
	}
	if file.RunID != "" {
		return nil, fmt.Errorf("recurring E2E requires generated run IDs")
	}
	if !file.Scenarios.MemberTransitions || file.Scenarios.LocalLifecycle {
		return nil, fmt.Errorf("recurring E2E requires memberTransitions and excludes localLifecycle")
	}
	if err := validateRecurringArtifacts(file.Artifacts.Root, options.repositoryRoot); err != nil {
		return nil, err
	}
	// Resolve both profiles before any network access. Each run receives its own
	// identity, while sharing the operator's exact cluster and cloud selections.
	configs := make([]resolvedConfig, 0, 2)
	for _, policy := range []corev1.ServiceExternalTrafficPolicy{
		corev1.ServiceExternalTrafficPolicyCluster,
		corev1.ServiceExternalTrafficPolicyLocal,
	} {
		file.Backend.ExternalTrafficPolicy = policy
		config, err := resolveFileConfig(file, options)
		if err != nil {
			return nil, fmt.Errorf("resolve %s profile: %w", policy, err)
		}
		configs = append(configs, config)
	}
	return configs, nil
}

func validateRecurringArtifacts(directory, repository string) error {
	if !filepath.IsAbs(directory) {
		return fmt.Errorf("recurring E2E requires an absolute artifacts.root outside the checkout")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("recurring artifacts.root must be an existing directory: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("recurring artifacts.root must be a directory")
	}
	repository, err = filepath.EvalSymlinks(repository)
	if err != nil {
		return fmt.Errorf("resolve recurring checkout: %w", err)
	}
	relative, err := filepath.Rel(repository, resolved)
	if err != nil || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("recurring artifacts.root must be outside the checkout so evidence survives checkout cleanup")
	}
	return nil
}

func runSequentially(ctx context.Context, configs []resolvedConfig, options runOptions, execute func(context.Context, resolvedConfig, runOptions) error) error {
	for _, config := range configs {
		if err := ctx.Err(); err != nil {
			return err
		}
		// run returns success only after the ownership audit and controller cleanup.
		// A failed or incomplete cleanup must prevent the next load balancer.
		if err := execute(ctx, config, options); err != nil {
			return err
		}
	}
	return nil
}
