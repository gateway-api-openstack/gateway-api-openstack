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
	"reflect"
	"testing"
)

func TestResolveNetworkScenariosRetainsExplicitScope(t *testing.T) {
	config := validFile()
	config.OpenStack.ProjectMode, config.OpenStack.DedicatedForE2E = ProjectModeDedicated, true
	config.OpenStack.AcceptProjectWideCredentialRisk = false
	config.OpenStack.ExternalNetworkID = "external-network"
	config.Kubernetes.ExpectedAPIServer = "https://api.example.test:6443"
	config.Kubernetes.ExpectedClusterUID = "33333333-3333-4333-8333-333333333333"
	config.Scenarios.FloatingIPRecovery = true
	config.Scenarios.FloatingIPDeletion = true
	config.OpenStack.Microversion = "2.29"
	config.OpenStack.FrontendSecurityGroupIDs = []string{"sg-b", "sg-a"}
	config.OpenStack.BackendSecurityGroupIDs = []string{"sg-worker"}
	resolved, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.FloatingIPRecovery || !resolved.FloatingIPDeletion || resolved.Audit.Microversion != "2.29" ||
		!reflect.DeepEqual(resolved.Project.FrontendSecurityGroupIDs, []string{"sg-a", "sg-b"}) ||
		!reflect.DeepEqual(resolved.Project.BackendSecurityGroupIDs, []string{"sg-worker"}) {
		t.Fatalf("resolved scope = %#v", resolved)
	}
	config.OpenStack.FrontendSecurityGroupIDs[0] = "changed"
	if !reflect.DeepEqual(resolved.Project.FrontendSecurityGroupIDs, []string{"sg-a", "sg-b"}) {
		t.Fatal("runtime references alias the user configuration")
	}
	for _, test := range []struct {
		name   string
		change func(*Runtime)
	}{
		{name: "shared project fault", change: func(c *Runtime) { c.Project.Mode = ProjectModeShared }},
		{name: "no address allocation", change: func(c *Runtime) { c.Project.ExpectedExternalNetworkID = "" }},
		{name: "no cluster fence", change: func(c *Runtime) { c.ExpectedAPIServer, c.ExpectedClusterUID = "", "" }},
		{name: "unsupported VIP API", change: func(c *Runtime) { c.Audit.Microversion = "2.28" }},
		{name: "malformed API version", change: func(c *Runtime) { c.Audit.Microversion = "2.+29" }},
		{name: "duplicate group IDs", change: func(c *Runtime) { c.Project.FrontendSecurityGroupIDs = []string{"sg-a", "sg-a"} }},
		{name: "resource path injection", change: func(c *Runtime) { c.Project.BackendSecurityGroupIDs = []string{"../ports/worker"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			modified := resolved
			test.change(&modified)
			if err := modified.Validate(); err == nil {
				t.Fatal("unsafe runtime scope was accepted")
			}
		})
	}
}

func TestReferencedSecurityDoesNotRequireFaultScope(t *testing.T) {
	config := validFile()
	config.OpenStack.ProjectMode, config.OpenStack.DedicatedForE2E, config.OpenStack.AcceptProjectWideCredentialRisk = ProjectModeShared, false, true
	config.OpenStack.Microversion = "2.29"
	config.OpenStack.FrontendSecurityGroupIDs = []string{"sg-frontend"}
	config.OpenStack.BackendSecurityGroupIDs = []string{"sg-worker"}
	resolved, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()})
	if err != nil || resolved.FloatingIPRecovery || resolved.Project.ExpectedExternalNetworkID != "" {
		t.Fatalf("shared project references = %#v, %v", resolved, err)
	}
}

func TestFloatingIPDeletionCannotBypassFaultScope(t *testing.T) {
	config := validFile()
	config.Scenarios.FloatingIPDeletion = true
	if _, err := Resolve(config, ResolveOptions{RepositoryRoot: t.TempDir()}); err == nil {
		t.Fatal("shared project accepted external address deletion")
	}
}
