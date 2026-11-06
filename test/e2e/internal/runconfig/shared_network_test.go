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
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSharedNetworkScopeRequiresExplicitBoundedProfile(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*File)
	}{
		{"missing opt-in", func(c *File) { c.OpenStack.AllowSharedNetworkTests = false }},
		{"dedicated project", func(c *File) {
			c.OpenStack.ProjectMode, c.OpenStack.DedicatedForE2E, c.OpenStack.AcceptProjectWideCredentialRisk = ProjectModeDedicated, true, false
		}},
		{"second lifecycle", func(c *File) { c.Scenarios.LocalLifecycle = true }},
		{"no member validation", func(c *File) { c.Scenarios.MemberTransitions = false }},
		{"Local policy", func(c *File) { c.Backend.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal }},
		{"one replica", func(c *File) { c.Backend.Replicas = 1 }},
		{"no selector", func(c *File) { c.Backend.NodeSelector = nil }},
		{"no cluster identity", func(c *File) { c.Kubernetes.ExpectedClusterUID = "" }},
		{"no API endpoint", func(c *File) { c.Kubernetes.ExpectedAPIServer = "" }},
		{"missing worker", func(c *File) { c.Backend.ExpectedWorkers = c.Backend.ExpectedWorkers[:1] }},
		{"no worker UID", func(c *File) { c.Backend.ExpectedWorkers[0].UID = "" }},
		{"no provider ID", func(c *File) { c.Backend.ExpectedWorkers[0].ProviderID = "" }},
		{"duplicate name", func(c *File) { c.Backend.ExpectedWorkers[1].Name = c.Backend.ExpectedWorkers[0].Name }},
		{"duplicate UID", func(c *File) { c.Backend.ExpectedWorkers[1].UID = c.Backend.ExpectedWorkers[0].UID }},
		{"duplicate server", func(c *File) { c.Backend.ExpectedWorkers[1].ProviderID = c.Backend.ExpectedWorkers[0].ProviderID }},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := sharedNetworkFile()
			test.change(&file)
			if _, err := Resolve(file, ResolveOptions{RepositoryRoot: t.TempDir()}); err == nil {
				t.Fatal("unsafe shared network profile was accepted")
			}
		})
	}
}

func TestSharedNetworkScopeSurvivesRuntimeValidation(t *testing.T) {
	file := sharedNetworkFile()
	config, err := Resolve(file, ResolveOptions{RepositoryRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !config.Project.AllowSharedNetworkTests || !reflect.DeepEqual(config.ExpectedWorkers, file.Backend.ExpectedWorkers) {
		t.Fatal("resolved profile lost its explicit scope")
	}
	file.Backend.ExpectedWorkers[0].UID = "changed"
	if config.ExpectedWorkers[0].UID == "changed" {
		t.Fatal("runtime worker identities alias the input file")
	}
	for _, change := range []func(*Runtime){
		func(c *Runtime) { c.Project.AllowSharedNetworkTests = false },
		func(c *Runtime) { c.ExpectedWorkers = nil },
		func(c *Runtime) { c.LocalLifecycle = true },
		func(c *Runtime) { c.MemberTransitions = false },
		func(c *Runtime) { c.Project.Mode = ProjectModeDedicated },
	} {
		modified := config
		change(&modified)
		if err := modified.Validate(); err == nil {
			t.Fatal("runtime widened the approved shared profile")
		}
	}
	for _, faults := range []Scenarios{
		{MemberTransitions: true},
		{MemberTransitions: true, FloatingIPRecovery: true},
		{MemberTransitions: true, FloatingIPDeletion: true},
		{MemberTransitions: true, FloatingIPRecovery: true, FloatingIPDeletion: true},
	} {
		file := sharedNetworkFile()
		file.Scenarios = faults
		if _, err := Resolve(file, ResolveOptions{RepositoryRoot: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSharedNetworkWorkersRejectIdentityAndEligibilityChanges(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func([]client.Object) []client.Object
		wantErr bool
	}{
		{name: "exact workers"},
		{name: "Node replaced", wantErr: true, change: func(o []client.Object) []client.Object { o[1].SetUID("replacement"); return o }},
		{name: "server changed", wantErr: true, change: func(o []client.Object) []client.Object {
			o[1].(*corev1.Node).Spec.ProviderID = "openstack:///other-server"
			return o
		}},
		{name: "worker missing", wantErr: true, change: func(o []client.Object) []client.Object { return o[:2] }},
		{name: "worker unready", wantErr: true, change: func(o []client.Object) []client.Object { o[1].(*corev1.Node).Status.Conditions = nil; return o }},
		{name: "worker unschedulable", wantErr: true, change: func(o []client.Object) []client.Object { o[1].(*corev1.Node).Spec.Unschedulable = true; return o }},
		{name: "selector changed", wantErr: true, change: func(o []client.Object) []client.Object { o[1].SetLabels(nil); return o }},
		{name: "worker excluded", wantErr: true, change: func(o []client.Object) []client.Object {
			o[1].GetLabels()[corev1.LabelNodeExcludeBalancers] = "true"
			return o
		}},
		{name: "foreign cluster", wantErr: true, change: func(o []client.Object) []client.Object { o[0].SetUID("foreign"); return o }},
		{name: "extra unready worker", wantErr: true, change: func(o []client.Object) []client.Object {
			return append(o, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "extra-worker"}})
		}},
		{name: "control plane", change: func(o []client.Object) []client.Object {
			return append(o, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "control-plane", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := Resolve(sharedNetworkFile(), ResolveOptions{RepositoryRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			objects := []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(config.ExpectedClusterUID)}}}
			for _, worker := range config.ExpectedWorkers {
				objects = append(objects, &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: worker.Name, UID: types.UID(worker.UID), Labels: map[string]string{"e2e.example.test/backend": "true"}},
					Spec:       corev1.NodeSpec{ProviderID: worker.ProviderID},
					Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
				})
			}
			if test.change != nil {
				objects = test.change(objects)
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			if err := RequireSharedNetworkWorkers(context.Background(), reader, config); (err != nil) != test.wantErr {
				t.Fatalf("RequireSharedNetworkWorkers() = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func sharedNetworkFile() File {
	file := validFile()
	file.OpenStack.AllowSharedNetworkTests = true
	file.OpenStack.ExternalNetworkID = "external-network"
	file.Kubernetes.ExpectedAPIServer = "https://api.example.test:6443"
	file.Kubernetes.ExpectedClusterUID = "33333333-3333-4333-8333-333333333333"
	file.Backend.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	file.Backend.NodeSelector = map[string]string{"e2e.example.test/backend": "true"}
	file.Backend.ExpectedWorkers = []WorkerIdentity{
		{Name: "worker-a", UID: "worker-a-uid", ProviderID: "openstack:///server-a"},
		{Name: "worker-b", UID: "worker-b-uid", ProviderID: "openstack:///server-b"},
	}
	file.Scenarios = Scenarios{MemberTransitions: true, FloatingIPRecovery: true, FloatingIPDeletion: true}
	return file
}
