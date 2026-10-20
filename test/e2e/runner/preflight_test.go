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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/runconfig"
)

func TestPreflightOnlyAuthenticatesAndAuditsWithoutResourceWrites(t *testing.T) {
	config, _, options := preparedRunFixture(t, runconfig.ProjectModeShared)
	options.preflightOnly = true
	// Preflight must not need installation manifests or invoke the live harness.
	options.repositoryRoot = t.TempDir()
	options.kubeClient = readOnlyClient(t)
	options.waitController = func(context.Context, client.Client, resolvedConfig, types.UID) error {
		t.Fatal("preflight waited for a controller installation")
		return nil
	}
	options.executeHarness = func(context.Context, resolvedConfig, runOptions) error {
		t.Fatal("preflight started the live harness")
		return nil
	}
	authentications := 0
	options.authenticateProject = func(context.Context, string, string, string, string) (string, error) {
		authentications++
		return config.Project.ExpectedProjectID, nil
	}
	audits := 0
	options.runAudit = func(context.Context, resolvedConfig, []string) error {
		audits++
		return nil
	}
	if err := run(context.Background(), config, options); err != nil {
		t.Fatal(err)
	}
	if authentications != 2 || audits != 1 {
		t.Fatalf("preflight performed %d authentications and %d audits", authentications, audits)
	}
}

func TestPreflightRejectsClusterMismatchBeforeCloudReadsOrInstallation(t *testing.T) {
	for _, preflightOnly := range []bool{false, true} {
		config, _, options := preparedRunFixture(t, runconfig.ProjectModeShared)
		config.ExpectedAPIServer = "https://approved.example.test"
		config.ExpectedClusterUID = "approved-uid"
		options.preflightOnly = preflightOnly
		options.kubeClient = readOnlyClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "foreign-uid"}})
		options.authenticateProject = func(context.Context, string, string, string, string) (string, error) {
			t.Fatal("cluster mismatch reached OpenStack authentication")
			return "", errors.New("unexpected authentication")
		}
		if err := run(context.Background(), config, options); err == nil {
			t.Fatal("run accepted a different cluster")
		}
	}
}

func readOnlyClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(runnerTestScheme(t)).WithObjects(append(objects, installedCRDs(t)...)...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("preflight attempted Create")
			return nil
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("preflight attempted Update")
			return nil
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			t.Fatal("preflight attempted Patch")
			return nil
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			t.Fatal("preflight attempted Delete")
			return nil
		},
		DeleteAllOf: func(context.Context, client.WithWatch, client.Object, ...client.DeleteAllOfOption) error {
			t.Fatal("preflight attempted DeleteAllOf")
			return nil
		},
	}).Build()
}
