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
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateExpectedCluster(apiServer, clusterUID string) error {
	if apiServer == "" && clusterUID == "" {
		return nil
	}
	endpoint, err := url.Parse(apiServer)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("kubernetes.expectedAPIServer must be an explicit HTTPS Kubernetes endpoint")
	}
	if clusterUID == "" || strings.TrimSpace(clusterUID) != clusterUID {
		return fmt.Errorf("kubernetes.expectedClusterUID must identify the kube-system Namespace")
	}
	return nil
}

// KubernetesRESTConfig loads the explicit context and verifies its endpoint
// before contacting the cluster. RequireClusterIdentity checks the live UID.
func KubernetesRESTConfig(config Runtime) (*rest.Config, error) {
	raw, err := clientcmd.LoadFromFile(config.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load explicit kubeconfig: %w", err)
	}
	selected, ok := raw.Contexts[config.KubeContext]
	if !ok || selected == nil {
		return nil, fmt.Errorf("explicit kubeconfig context does not exist")
	}
	cluster, ok := raw.Clusters[selected.Cluster]
	if !ok || cluster == nil {
		return nil, fmt.Errorf("explicit kubeconfig context has no cluster")
	}
	if config.ExpectedAPIServer != "" && strings.TrimRight(cluster.Server, "/") != strings.TrimRight(config.ExpectedAPIServer, "/") {
		return nil, fmt.Errorf("selected context does not match the expected API server")
	}
	result, err := clientcmd.NewNonInteractiveClientConfig(*raw, config.KubeContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("resolve explicit kubeconfig context: %w", err)
	}
	result.Timeout = config.HTTPTimeout
	return result, nil
}

// RequireClusterIdentity compares the live kube-system UID when the run pins a
// target cluster. It makes no changes and must precede installation or fixtures.
func RequireClusterIdentity(ctx context.Context, reader client.Reader, config Runtime) error {
	if config.ExpectedClusterUID == "" {
		return nil
	}
	var system corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: "kube-system"}, &system); err != nil {
		return fmt.Errorf("read cluster identity: %w", err)
	}
	if string(system.UID) != config.ExpectedClusterUID || !system.DeletionTimestamp.IsZero() {
		return fmt.Errorf("cluster identity does not match the expected cluster")
	}
	return nil
}
