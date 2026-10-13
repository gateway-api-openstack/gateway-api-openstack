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
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	optInEnvironment  = "GATEWAY_OPENSTACK_CONFORMANCE_DIAGNOSTIC"
	configEnvironment = "GATEWAY_OPENSTACK_CONFORMANCE_CONFIG"
	testNamespace     = "gateway-conformance-infra"
	gatewayName       = "same-namespace"
	backendName       = "infra-backend-v1"
	routeName         = "gateway-conformance-infra-test"
	runAnnotation     = "diagnostic.gateway-api-openstack.github.io/run-id"
	upstreamVersion   = "v1.6.1"
	upstreamRevision  = "8bb74df00e56ec8f944d48c25e6c1c9c2f6848e3"
	selectedTest      = "HTTPRouteSimpleSameNamespace"
)

// Config names existing infrastructure. The runner creates only its test fixture.
// ClusterUID is the UID of kube-system, independent of kubeconfig display names.
// AcceptResourceCreation acknowledges one Gateway graph and its normal cleanup.
type Config struct {
	Kubeconfig             string            `json:"kubeconfig"`
	Context                string            `json:"context"`
	APIServer              string            `json:"apiServer"`
	ClusterUID             string            `json:"clusterUID"`
	Namespace              string            `json:"namespace"`
	ControllerName         string            `json:"controllerName"`
	GatewayClass           string            `json:"gatewayClass"`
	GatewayClassUID        string            `json:"gatewayClassUID"`
	ClassConfig            string            `json:"classConfig"`
	ClassConfigUID         string            `json:"classConfigUID"`
	BackendImage           string            `json:"backendImage"`
	BackendNodeSelector    map[string]string `json:"backendNodeSelector"`
	ReportPath             string            `json:"reportPath"`
	AcceptResourceCreation bool              `json:"acceptResourceCreation"`
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	if !filepath.IsAbs(path) {
		return cfg, fmt.Errorf("configuration path must be absolute")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read diagnostic configuration: %w", err)
	}
	if len(data) > 64<<10 {
		return cfg, fmt.Errorf("diagnostic configuration exceeds 64 KiB")
	}
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return cfg, fmt.Errorf("decode diagnostic configuration: %w", err)
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if !c.AcceptResourceCreation {
		return fmt.Errorf("acceptResourceCreation must explicitly approve one Gateway graph and cleanup")
	}
	if !filepath.IsAbs(c.Kubeconfig) || !filepath.IsAbs(c.ReportPath) || c.Kubeconfig == c.ReportPath {
		return fmt.Errorf("kubeconfig and reportPath must be distinct absolute paths")
	}
	endpoint, err := url.Parse(c.APIServer)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("apiServer must be an explicit HTTPS Kubernetes endpoint")
	}
	for _, value := range []string{c.Context, c.ClusterUID, c.ControllerName, c.GatewayClassUID, c.ClassConfigUID} {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("context and expected cluster, class, parameter, and controller identities are required")
		}
	}
	if !strings.Contains(c.ControllerName, "/") {
		return fmt.Errorf("controllerName must include its domain and path")
	}
	if c.Namespace != testNamespace {
		return fmt.Errorf("namespace must be exactly %s for the selected upstream assertion", testNamespace)
	}
	if len(validation.IsDNS1123Subdomain(c.GatewayClass)) != 0 || len(validation.IsDNS1123Subdomain(c.ClassConfig)) != 0 {
		return fmt.Errorf("gatewayClass and classConfig must be valid explicit names")
	}
	if !regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`).MatchString(c.BackendImage) {
		return fmt.Errorf("backendImage must identify the upstream echo-basic backend pinned by sha256 digest")
	}
	if len(c.BackendNodeSelector) == 0 {
		return fmt.Errorf("backendNodeSelector must explicitly restrict backend placement")
	}
	for key, value := range c.BackendNodeSelector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("backendNodeSelector contains an invalid label")
		}
	}
	return nil
}
