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
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesRESTConfigRequiresSelectedEndpoint(t *testing.T) {
	config := validRuntime(t)
	config.Kubeconfig = filepath.Join(t.TempDir(), "kubeconfig")
	config.KubeContext = "approved"
	config.ExpectedAPIServer = "https://approved.example.test:6443"
	raw := clientcmdapi.Config{
		CurrentContext: "foreign",
		Contexts: map[string]*clientcmdapi.Context{
			"approved": {Cluster: "approved", AuthInfo: "test"},
			"foreign":  {Cluster: "foreign", AuthInfo: "test"},
		},
		Clusters: map[string]*clientcmdapi.Cluster{
			"approved": {Server: config.ExpectedAPIServer + "/"},
			"foreign":  {Server: "https://foreign.example.test:6443"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"test": {Token: "synthetic-test-token"}},
	}
	if err := clientcmd.WriteToFile(raw, config.Kubeconfig); err != nil {
		t.Fatal(err)
	}
	selected, err := KubernetesRESTConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Host != config.ExpectedAPIServer+"/" || selected.Timeout != config.HTTPTimeout {
		t.Fatal("Kubernetes client did not retain the explicit endpoint or request timeout")
	}
	config.KubeContext = "foreign"
	if _, err := KubernetesRESTConfig(config); err == nil || !strings.Contains(err.Error(), "expected API server") {
		t.Fatalf("foreign context = %v", err)
	}
	config.KubeContext = "missing"
	if _, err := KubernetesRESTConfig(config); err == nil || !strings.Contains(err.Error(), "context does not exist") {
		t.Fatalf("missing context = %v", err)
	}
}

func TestRequireClusterIdentityRejectsDifferentOrMissingCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	config := Runtime{ExpectedClusterUID: "approved-uid"}
	for _, test := range []struct {
		name      string
		namespace *corev1.Namespace
		wantErr   bool
	}{
		{name: "approved", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "approved-uid"}}},
		{name: "foreign", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "foreign-uid"}}, wantErr: true},
		{name: "missing", wantErr: true},
		{name: "deleting", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "approved-uid", DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time}, Finalizers: []string{"test"}}}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if test.namespace != nil {
				builder.WithObjects(test.namespace)
			}
			if err := RequireClusterIdentity(context.Background(), builder.Build(), config); (err != nil) != test.wantErr {
				t.Fatalf("RequireClusterIdentity() = %v, want error %t", err, test.wantErr)
			}
		})
	}
}
