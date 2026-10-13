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

package httproute

import (
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestSetupRequiresGraphWriter(t *testing.T) {
	kubeClient := indexedFakeClientBuilder(testScheme(t), testConfig()).Build()
	reconciler := &Reconciler{APIReader: kubeClient, Coordinator: &graph.Coordinator{}}
	if err := reconciler.SetupWithManager(nil); err == nil || err.Error() != "gateway graph writer is required" {
		t.Fatalf("SetupWithManager() error = %v", err)
	}
}
