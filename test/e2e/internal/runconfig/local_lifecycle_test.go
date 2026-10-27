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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestResolveLocalLifecycleRequiresExactTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*File)
		want   string
	}{
		{name: "one worker", change: func(config *File) { config.Backend.Replicas = 1 }},
		{name: "default two workers", change: func(config *File) { config.Backend.Replicas = 0 }},
		{name: "dedicated project", change: func(config *File) {
			config.OpenStack.ProjectMode = ProjectModeDedicated
			config.OpenStack.DedicatedForE2E = true
			config.OpenStack.AcceptProjectWideCredentialRisk = false
		}},
		{name: "floating IP", change: func(config *File) { config.OpenStack.ExternalNetworkID = "external-network" }, want: "without Floating IP"},
		{name: "Cluster policy", change: func(config *File) {
			config.Backend.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
			config.Backend.NodeSelector = nil
		}, want: "Local backend traffic policy"},
		{name: "missing selector", change: func(config *File) { config.Backend.NodeSelector = nil }, want: "explicit Node selector"},
		{name: "missing identity", change: func(config *File) {
			config.Kubernetes.ExpectedAPIServer = ""
			config.Kubernetes.ExpectedClusterUID = ""
		}, want: "expectedAPIServer and kubernetes.expectedClusterUID"},
		{name: "missing UID", change: func(config *File) { config.Kubernetes.ExpectedClusterUID = "" }, want: "expectedClusterUID"},
		{name: "HTTP endpoint", change: func(config *File) { config.Kubernetes.ExpectedAPIServer = "http://cluster.example.test" }, want: "HTTPS"},
		{name: "URL credentials", change: func(config *File) { config.Kubernetes.ExpectedAPIServer = "https://secret@cluster.example.test" }, want: "HTTPS"},
		{name: "too many replicas", change: func(config *File) { config.Backend.Replicas = 3 }, want: "replicas must be 1 or 2"},
		{name: "negative replicas", change: func(config *File) { config.Backend.Replicas = -1 }, want: "replicas must be 1 or 2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := localLifecycleFile()
			test.change(&config)
			resolved, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()})
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("Resolve() = %v, want %q", err, test.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !resolved.LocalLifecycle || resolved.ExpectedAPIServer != config.Kubernetes.ExpectedAPIServer ||
				resolved.ExpectedClusterUID != config.Kubernetes.ExpectedClusterUID || resolved.BackendReplicas != backendReplicas(config.Backend.Replicas) {
				t.Fatal("resolved configuration lost Local lifecycle target or backend limits")
			}
		})
	}
}

func TestRuntimeLocalLifecycleCannotBypassFilePolicy(t *testing.T) {
	resolved, err := Resolve(localLifecycleFile(), ResolveOptions{RepositoryRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Runtime){
		func(config *Runtime) { config.Project.ExpectedExternalNetworkID = "external-network" },
		func(config *Runtime) { config.BackendNodeSelector = nil },
		func(config *Runtime) {
			config.BackendExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
		},
		func(config *Runtime) { config.ExpectedAPIServer, config.ExpectedClusterUID = "", "" },
		func(config *Runtime) { config.BackendReplicas = 0 },
	} {
		modified := resolved
		change(&modified)
		if err := modified.Validate(); err == nil {
			t.Fatal("runtime validation accepted an unsafe Local lifecycle configuration")
		}
	}
}

func localLifecycleFile() File {
	config := validFile()
	config.Scenarios.LocalLifecycle = true
	config.Kubernetes.ExpectedAPIServer = "https://cluster.example.test:6443"
	config.Kubernetes.ExpectedClusterUID = "cluster-uid"
	config.Backend.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	config.Backend.NodeSelector = map[string]string{"e2e.example.test/backend": "true"}
	return config
}

func TestResolveMemberTransitionsScope(t *testing.T) {
	for _, test := range []struct {
		name     string
		change   func(*File)
		rejected bool
	}{
		{name: "shared Local"},
		{name: "dedicated Cluster", change: func(c *File) {
			c.OpenStack.ProjectMode, c.OpenStack.DedicatedForE2E, c.OpenStack.AcceptProjectWideCredentialRisk = ProjectModeDedicated, true, false
			c.Backend.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
		}},
		{name: "shared Cluster", change: func(c *File) { c.Backend.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster }, rejected: true},
		{name: "missing selector", change: func(c *File) { c.Backend.NodeSelector = nil }, rejected: true},
		{name: "one replica", change: func(c *File) { c.Backend.Replicas = 1 }, rejected: true},
		{name: "no cluster fence", change: func(c *File) { c.Kubernetes.ExpectedAPIServer, c.Kubernetes.ExpectedClusterUID = "", "" }, rejected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := localLifecycleFile()
			config.Scenarios.LocalLifecycle, config.Scenarios.MemberTransitions = false, true
			if test.change != nil {
				test.change(&config)
			}
			resolved, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()})
			if (err != nil) != test.rejected {
				t.Fatalf("Resolve() = %v", err)
			}
			if !test.rejected && !resolved.MemberTransitions {
				t.Fatal("member scenario selection lost")
			}
		})
	}
}
