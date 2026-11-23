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

package runconfig

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
)

func TestHTTPScenariosPreserveExplicitEndpointProfile(t *testing.T) {
	file := validFile()
	file.Kubernetes.ExpectedAPIServer = "https://api.example.test:6443"
	file.Kubernetes.ExpectedClusterUID = "33333333-3333-4333-8333-333333333333"
	file.Backend.MemberMode = configv1alpha1.MemberModeEndpoint
	file.Scenarios.HTTPRouting, file.Scenarios.EndpointBackends = true, true
	runtime, err := Resolve(file, ResolveOptions{RepositoryRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.BackendMemberMode != configv1alpha1.MemberModeEndpoint || runtime.BackendExternalTrafficPolicy != "" || !runtime.HTTPRouting || !runtime.EndpointBackends {
		t.Fatalf("Endpoint profile lost: %#v", runtime)
	}
	for _, test := range []struct {
		name   string
		change func(*Runtime)
	}{
		{"NodePort policy", func(c *Runtime) { c.BackendExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal }},
		{"worker transitions", func(c *Runtime) { c.MemberTransitions = true }},
		{"worker security", func(c *Runtime) { c.Project.BackendSecurityGroupIDs = []string{"worker-sg"} }},
		{"unpinned cluster", func(c *Runtime) { c.ExpectedAPIServer, c.ExpectedClusterUID = "", "" }},
		{"unknown mode", func(c *Runtime) { c.BackendMemberMode = "Unknown" }},
		{"NodePort-only endpoint check", func(c *Runtime) {
			c.BackendMemberMode = configv1alpha1.MemberModeNodePort
			c.BackendExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := runtime
			test.change(&changed)
			if changed.Validate() == nil {
				t.Fatal("invalid Endpoint scope accepted")
			}
		})
	}
}

func TestHTTPRoutingRequiresPinnedCluster(t *testing.T) {
	runtime := validRuntime(t)
	runtime.HTTPRouting = true
	if runtime.Validate() == nil {
		t.Fatal("HTTP routing test accepted an unpinned cluster")
	}
	runtime.ExpectedAPIServer = "https://api.example.test:6443"
	runtime.ExpectedClusterUID = "33333333-3333-4333-8333-333333333333"
	if err := runtime.Validate(); err != nil {
		t.Fatal(err)
	}
}
