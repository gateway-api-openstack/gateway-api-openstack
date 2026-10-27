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
	"fmt"
	"io"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
)

type commandOptions struct {
	classConfig  string
	kubeconfig   string
	kubeContext  string
	nodeSelector labels.Selector
	barbican     bool
	timeout      time.Duration
	openStack    clients.ClientConfig
}

func parseOptions(args []string, getenv func(string) string, stderr io.Writer) (commandOptions, error) {
	options := commandOptions{}
	var selector string
	flags := pflag.NewFlagSet("openstack-gateway-diagnostics", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.classConfig, "class-config", "", "existing OpenStackGatewayClassConfig to inspect (required)")
	flags.StringVar(&options.kubeconfig, "kubeconfig", "", "kubeconfig path; defaults to standard client-go loading")
	flags.StringVar(&options.kubeContext, "context", "", "kubeconfig context")
	flags.StringVar(&selector, "node-selector", "", "optional label selector to restrict worker Node reads")
	flags.BoolVar(&options.barbican, "check-barbican", false, "check Barbican metadata list access without reading secret payloads")
	flags.DurationVar(&options.timeout, "timeout", 2*time.Minute, "maximum duration of all input reads and diagnostic checks")
	flags.StringVar(&options.openStack.CloudsYAMLPath, "clouds-yaml", getenv("OS_CLIENT_CONFIG_FILE"), "path to clouds.yaml; defaults to OS_CLIENT_CONFIG_FILE")
	flags.StringVar(&options.openStack.CloudName, "openstack-cloud", getenv("OS_CLOUD"), "clouds.yaml entry; defaults to OS_CLOUD")
	flags.StringVar(&options.openStack.Region, "openstack-region", getenv("OS_REGION_NAME"), "OpenStack region; defaults to OS_REGION_NAME")
	flags.StringVar(&options.openStack.Microversion, "octavia-microversion", "2.5", "configured Octavia API microversion")
	flags.Float64Var(&options.openStack.APIQPS, "openstack-api-qps", clients.DefaultAPIQPS, "maximum average OpenStack API requests per second")
	flags.IntVar(&options.openStack.APIBurst, "openstack-api-burst", clients.DefaultAPIBurst, "maximum OpenStack API request burst")
	flags.BoolVar(&options.openStack.AllowInsecure, "insecure", false, "disable OpenStack TLS verification; test clouds only")
	if err := flags.Parse(args); err != nil {
		if err == pflag.ErrHelp {
			flags.SetOutput(stderr)
			_, _ = fmt.Fprintln(stderr, "Read-only OpenStack environment diagnostics\n\nUsage:\n  openstack-gateway-diagnostics --class-config <name> [flags]\n\nWrites one redacted JSON report. Unknown checks are not evidence of support.\nExit status: 0 for no observed failures, 1 for command errors, 2 for failed checks.")
			flags.PrintDefaults()
			return commandOptions{}, err
		}
		return commandOptions{}, fmt.Errorf("invalid command options")
	}
	if flags.NArg() != 0 || options.classConfig == "" || len(validation.IsDNS1123Subdomain(options.classConfig)) != 0 {
		return commandOptions{}, fmt.Errorf("a valid --class-config name is required and positional arguments are not supported")
	}
	if options.timeout <= 0 {
		return commandOptions{}, fmt.Errorf("timeout must be greater than zero")
	}
	var err error
	options.nodeSelector, err = labels.Parse(selector)
	if err != nil {
		return commandOptions{}, fmt.Errorf("node selector is invalid")
	}
	if err := options.openStack.Validate(); err != nil {
		return commandOptions{}, fmt.Errorf("OpenStack client configuration is invalid")
	}
	return options, nil
}
