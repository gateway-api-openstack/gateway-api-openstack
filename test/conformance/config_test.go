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
	"os"
	"path/filepath"
	"testing"
	"time"

	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func testConfig() Config {
	return Config{
		FormatVersion: "v1alpha1", Mode: profileMode,
		Kubeconfig: "/private/kubeconfig", Context: "conformance",
		ExpectedAPIServer: "https://cluster.example.test", ExpectedClusterUID: "cluster-uid",
		DedicatedCluster: true, DedicatedOpenStackProject: true, AcceptUpstreamResourceFootprint: true,
		GatewayClassName: "test-openstack", ControllerName: "example.test/gateway-api-openstack",
		ArtifactsDirectory: "/private/conformance-run",
		Implementation:     confv1.Implementation{Organization: "example", Project: "gateway-api-openstack", URL: "https://example.test", Version: "v0.5.0-dev", Contact: []string{"https://example.test/issues"}},
	}
}

func TestConfigurationRejectsUnsafeOrMisleadingRuns(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"shared cluster", func(c *Config) { c.DedicatedCluster = false }},
		{"shared project", func(c *Config) { c.DedicatedOpenStackProject = false }},
		{"missing footprint approval", func(c *Config) { c.AcceptUpstreamResourceFootprint = false }},
		{"implicit kubeconfig", func(c *Config) { c.Kubeconfig = "" }},
		{"implicit context", func(c *Config) { c.Context = "" }},
		{"unpinned cluster", func(c *Config) { c.ExpectedClusterUID = "" }},
		{"insecure server", func(c *Config) { c.ExpectedAPIServer = "http://cluster.example.test" }},
		{"URL credentials", func(c *Config) { c.ExpectedAPIServer = "https://user:secret@cluster.example.test" }},
		{"profile feature override", func(c *Config) { c.Diagnostic = &Diagnostic{} }},
		{"profile timing bypass", func(c *Config) { c.TimeoutConfig.MaxTimeToConsistency = time.Minute }},
		{"implicit diagnostic features", func(c *Config) { c.Mode = diagnosticMode }},
		{"missing Core feature", func(c *Config) {
			c.Mode = diagnosticMode
			c.Diagnostic = &Diagnostic{EvaluationFeatures: []features.FeatureName{features.SupportGateway}}
		}},
		{"foreign profile feature", func(c *Config) {
			c.Mode = diagnosticMode
			c.Diagnostic = &Diagnostic{EvaluationFeatures: []features.FeatureName{features.SupportGateway, features.SupportHTTPRoute, features.SupportReferenceGrant, features.SupportTCPRoute}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			test.change(&config)
			if err := config.validate(); err == nil {
				t.Fatal("unsafe run accepted")
			}
		})
	}
}

func TestProfileAndDiagnosticSelection(t *testing.T) {
	config := testConfig()
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	profile := config.options()
	if len(profile.SupportedFeatures) != 0 || !profile.CleanupBaseResources || !profile.CleanupTestResources || profile.AllowCRDsMismatch || len(profile.SkipTests) != 0 {
		t.Fatalf("profile changes the upstream feature or cleanup contract: %#v", profile)
	}
	config.Mode = diagnosticMode
	config.Diagnostic = &Diagnostic{EvaluationFeatures: []features.FeatureName{features.SupportGateway, features.SupportHTTPRoute, features.SupportReferenceGrant}}
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	diagnostic := config.options()
	if len(diagnostic.SupportedFeatures) != 3 || diagnostic.RunTest != "" || len(diagnostic.SkipTests) != 0 {
		t.Fatalf("full Core diagnostic was narrowed: %#v", diagnostic)
	}
}

func TestSingleDiagnosticCannotBroadenFeatureScope(t *testing.T) {
	config := testConfig()
	config.Mode = diagnosticMode
	config.Diagnostic = &Diagnostic{EvaluationFeatures: []features.FeatureName{features.SupportGateway, features.SupportHTTPRoute, features.SupportReferenceGrant}}
	tests := []suite.ConformanceTest{
		{ShortName: "HTTP", Features: []features.FeatureName{features.SupportGateway, features.SupportHTTPRoute}},
		{ShortName: "TCP", Features: []features.FeatureName{features.SupportGateway, features.SupportTCPRoute}},
	}
	for _, name := range []string{"HTTP", "TCP", "Missing"} {
		config.Diagnostic.RunTest = name
		if err := validateDiagnosticTest(config, tests); (err != nil) != (name != "HTTP") {
			t.Errorf("%s selection error = %v", name, err)
		}
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("formatVersion: v1alpha1\nmode: profile\nallowSharedProject: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("unknown safety field accepted")
	}
}

func TestKubeconfigUsesOnlyExplicitContext(t *testing.T) {
	config := testConfig()
	config.Kubeconfig = filepath.Join(t.TempDir(), "kubeconfig")
	data := `apiVersion: v1
kind: Config
current-context: other
clusters:
- name: expected
  cluster:
    server: https://cluster.example.test
- name: other
  cluster:
    server: https://other.example.test
contexts:
- name: conformance
  context:
    cluster: expected
- name: other
  context:
    cluster: other
`
	if err := os.WriteFile(config.Kubeconfig, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	rest, err := kubernetesConfig(config)
	if err != nil || rest.Host != config.ExpectedAPIServer {
		t.Fatalf("explicit context: %#v, %v", rest, err)
	}
	config.Context = "other"
	if _, err := kubernetesConfig(config); err == nil {
		t.Fatal("context pointing to another cluster accepted")
	}
}
