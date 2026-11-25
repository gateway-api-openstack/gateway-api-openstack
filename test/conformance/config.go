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

package conformance

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	conformanceconfig "sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
	"sigs.k8s.io/yaml"
)

const (
	bundleVersion     = "v1.6.1"
	enableEnvironment = "GATEWAY_OPENSTACK_CONFORMANCE"
	configEnvironment = "GATEWAY_OPENSTACK_CONFORMANCE_CONFIG"
	profileMode       = "profile"
	diagnosticMode    = "diagnostic"
	runLabel          = "gateway-api-openstack.github.io/conformance-run"
	lockNamespace     = "gateway-api-openstack-conformance"
)

// Config selects an installed controller and an explicitly dedicated test environment.
// OpenStack acknowledgements describe operator approval, not a cloud identity probe.
type Config struct {
	FormatVersion                   string                          `json:"formatVersion"`
	Mode                            string                          `json:"mode"`
	Kubeconfig                      string                          `json:"kubeconfig"`
	Context                         string                          `json:"context"`
	ExpectedAPIServer               string                          `json:"expectedAPIServer"`
	ExpectedClusterUID              string                          `json:"expectedClusterUID"`
	DedicatedCluster                bool                            `json:"dedicatedCluster"`
	DedicatedOpenStackProject       bool                            `json:"dedicatedOpenStackProject"`
	AcceptUpstreamResourceFootprint bool                            `json:"acceptUpstreamResourceFootprint"`
	GatewayClassName                string                          `json:"gatewayClassName"`
	ControllerName                  string                          `json:"controllerName"`
	ArtifactsDirectory              string                          `json:"artifactsDirectory"`
	Implementation                  confv1.Implementation           `json:"implementation"`
	Diagnostic                      *Diagnostic                     `json:"diagnostic,omitempty"`
	TimeoutConfig                   conformanceconfig.TimeoutConfig `json:"timeoutConfig,omitempty"`
}

// Diagnostic selects evaluation features without modifying advertised support.
// An empty RunTest evaluates the entire profile, including all its Core tests.
type Diagnostic struct {
	EvaluationFeatures []features.FeatureName `json:"evaluationFeatures"`
	RunTest            string                 `json:"runTest,omitempty"`
}

func loadConfig(path string) (Config, error) {
	var config Config
	if !filepath.IsAbs(path) {
		return config, fmt.Errorf("conformance configuration path must be absolute")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("read conformance configuration: %w", err)
	}
	if err := yaml.UnmarshalStrict(data, &config); err != nil {
		return config, fmt.Errorf("decode conformance configuration: %w", err)
	}
	if err := config.validate(); err != nil {
		return config, err
	}
	conformanceconfig.SetupTimeoutConfig(&config.TimeoutConfig)
	return config, nil
}

func (c Config) validate() error {
	if c.FormatVersion != "v1alpha1" || c.Mode != profileMode && c.Mode != diagnosticMode {
		return fmt.Errorf("formatVersion must be v1alpha1 and mode must be profile or diagnostic")
	}
	if !c.DedicatedCluster || !c.DedicatedOpenStackProject || !c.AcceptUpstreamResourceFootprint {
		return fmt.Errorf("conformance requires a dedicated cluster, a dedicated OpenStack project, and approval for the unmodified upstream resource footprint; shared projects are unsupported")
	}
	if !filepath.IsAbs(c.Kubeconfig) || strings.TrimSpace(c.Context) == "" || strings.TrimSpace(c.ExpectedClusterUID) == "" {
		return fmt.Errorf("an absolute kubeconfig, explicit context, and expectedClusterUID are required")
	}
	endpoint, err := url.Parse(c.ExpectedAPIServer)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("expectedAPIServer must be an explicit HTTPS endpoint without credentials or query parameters")
	}
	if c.GatewayClassName == "" || c.ControllerName == "" || !filepath.IsAbs(c.ArtifactsDirectory) {
		return fmt.Errorf("gatewayClassName, controllerName, and an absolute artifactsDirectory are required")
	}
	if err := c.Implementation.Validate(); err != nil {
		return fmt.Errorf("implementation metadata: %w", err)
	}
	if c.Mode == profileMode {
		if c.Diagnostic != nil {
			return fmt.Errorf("profile mode infers features from GatewayClass and cannot select or skip tests")
		}
		if c.TimeoutConfig.MaxTimeToConsistency > 30*time.Second {
			return fmt.Errorf("profile mode cannot increase the upstream 30s consistency limit")
		}
		return nil
	}
	if c.Diagnostic == nil {
		return fmt.Errorf("diagnostic mode requires explicit evaluationFeatures")
	}
	selected := sets.New(c.Diagnostic.EvaluationFeatures...)
	profile := suite.GatewayHTTPConformanceProfile
	if !selected.IsSuperset(profile.CoreFeatures) {
		return fmt.Errorf("diagnostic evaluationFeatures must include every GATEWAY-HTTP Core feature")
	}
	if !profile.CoreFeatures.Union(profile.ExtendedFeatures).IsSuperset(selected) {
		return fmt.Errorf("evaluationFeatures must belong to the pinned GATEWAY-HTTP profile")
	}
	if len(selected) != len(c.Diagnostic.EvaluationFeatures) {
		return fmt.Errorf("evaluationFeatures must not contain duplicates")
	}
	return nil
}

func (c Config) options() suite.ConfigurableOptions {
	options := suite.ConfigurableOptions{
		GatewayClassName:     c.GatewayClassName,
		CleanupBaseResources: true,
		CleanupTestResources: true,
		ConformanceProfiles:  []suite.ConformanceProfileName{suite.GatewayHTTPConformanceProfileName},
		Implementation:       c.Implementation,
		Mode:                 "default",
		TimeoutConfig:        c.TimeoutConfig,
	}
	if c.Diagnostic != nil {
		options.SupportedFeatures = slices.Clone(c.Diagnostic.EvaluationFeatures)
		options.RunTest = c.Diagnostic.RunTest
	}
	return options
}

func validateDiagnosticTest(config Config, tests []suite.ConformanceTest) error {
	if config.Diagnostic == nil || config.Diagnostic.RunTest == "" {
		return nil
	}
	for _, test := range tests {
		if test.ShortName != config.Diagnostic.RunTest {
			continue
		}
		if !sets.New(config.Diagnostic.EvaluationFeatures...).HasAll(test.Features...) {
			return fmt.Errorf("diagnostic runTest requires features outside the selected evaluation scope")
		}
		return nil
	}
	return fmt.Errorf("diagnostic runTest is not in the pinned upstream suite")
}
