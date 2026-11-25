//go:build conformance

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
	"context"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	upstream "sigs.k8s.io/gateway-api/conformance"
)

func TestFixtureIsolationIncludesNamespacesAndClusterScopedObjects(t *testing.T) {
	fixtures, err := isolationFixtures(upstream.Manifests)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures {
		names[fixture.GetObjectKind().GroupVersionKind().Kind+"/"+fixture.GetName()] = true
	}
	for _, name := range []string{"Namespace/gateway-conformance-infra", "Namespace/gateway-conformance-app-backend", "Namespace/gateway-conformance-web-backend", "GatewayClass/gatewayclass-observed-generation-bump"} {
		if !names[name] {
			t.Errorf("missing upstream isolation boundary %s", name)
		}
	}
	invalid := fstest.MapFS{"base/manifests.yaml": {Data: []byte("kind: [invalid\n")}}
	if _, err := isolationFixtures(invalid); err == nil {
		t.Fatal("malformed upstream inventory accepted")
	}
}

func TestCleanupDoesNotRemoveFinalizers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gateway-conformance-infra", Finalizers: []string{"example.test/retain"}}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := waitForFixtureCleanup(ctx, kubeClient, []client.Object{namespace}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected blocked cleanup, got %v", err)
	}
	var current corev1.Namespace
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(namespace), &current); err != nil {
		t.Fatal(err)
	}
	if len(current.Finalizers) != 1 || current.Finalizers[0] != "example.test/retain" {
		t.Fatal("cleanup changed finalizers")
	}
}
