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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/pflag"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
	openstackdiagnostics "github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/diagnostics"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/diagnostics"
)

type commandDependencies struct {
	readInputs   func(context.Context, commandOptions) (diagnostics.Request, error)
	newInspector func(context.Context, clients.ClientConfig, time.Duration) (diagnostics.Inspector, error)
}

func productionDependencies() commandDependencies {
	return commandDependencies{
		readInputs: readInputs,
		newInspector: func(ctx context.Context, config clients.ClientConfig, timeout time.Duration) (diagnostics.Inspector, error) {
			serviceClients, err := clients.NewServiceClients(ctx, config)
			if err != nil {
				return nil, err
			}
			return openstackdiagnostics.NewScanner(serviceClients, timeout), nil
		},
	}
}

func runCommand(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, dependencies commandDependencies) int {
	options, err := parseOptions(args, getenv, stderr)
	if errors.Is(err, pflag.ErrHelp) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()
	report := executeDiagnostics(ctx, options, dependencies)
	report.ToolVersion = version
	report.GeneratedAt = time.Now().UTC()
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		_, _ = fmt.Fprintln(stderr, "could not write diagnostic report")
		return 1
	}
	if report.HasFailures() {
		return 2
	}
	return 0
}

func executeDiagnostics(ctx context.Context, options commandOptions, dependencies commandDependencies) diagnostics.Report {
	request, err := dependencies.readInputs(ctx, options)
	if err != nil {
		return failedReport("kubernetesInput", "The class configuration and selected worker Nodes could not be read and validated.")
	}
	inspector, err := dependencies.newInspector(ctx, options.openStack, options.timeout)
	if err != nil {
		return failedReport("authentication", "Project-scoped OpenStack authentication and service discovery could not be completed.")
	}
	return inspector.Inspect(ctx, request)
}

func failedReport(name, message string) diagnostics.Report {
	report := diagnostics.NewReport()
	report.Checks = append(report.Checks, diagnostics.Check{Name: name, Status: diagnostics.Failed, Message: message})
	return report
}
