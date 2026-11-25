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
	"context"
	"errors"
	"maps"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func TestBoundRouteGraphBuildsBackendModelOnce(t *testing.T) {
	f := newRouteModelInputFixture(t)
	provider := &recordingProvider{}
	f.reconciler.Coordinator = &graph.Coordinator{}
	r := newTestReconciler(*f.reconciler, provider)

	for i := range 2 {
		clear(f.reader.counts)
		result, err := r.ensureRouteGraph(t.Context(), f.route, f.gateway)
		if err != nil || result.bindingState != routeBindingReady || result.outcome.State != cloud.OutcomeReady {
			t.Fatalf("ensure bound graph = %#v, %v", result, err)
		}
		// One model observation and one final dependency fence remain necessary.
		want := map[string]int{"Service": 2, "EndpointSlice": 2}
		if !maps.Equal(f.reader.counts, want) {
			t.Fatalf("backend reads = %v, want %v", f.reader.counts, want)
		}
		if len(provider.Calls) != i+1 {
			t.Fatalf("graph requests = %d, want %d", len(provider.Calls), i+1)
		}
	}
}

func TestBoundRouteGraphRetainsFinalBackendFence(t *testing.T) {
	f := newRouteModelInputFixture(t)
	changed := false
	reader := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, kube client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := kube.List(ctx, list, opts...); err != nil {
				return err
			}
			if _, ok := list.(*discoveryv1.EndpointSliceList); !ok || changed {
				return nil
			}
			// Change a dependency after its first observation, before the provider fence.
			changed = true
			f.endpoints.Endpoints[0].Addresses = []string{"192.0.2.99"}
			return kube.Update(ctx, f.endpoints)
		},
	})
	f.reconciler.APIReader = reader
	f.reconciler.Coordinator = &graph.Coordinator{}
	provider := &recordingProvider{}
	r := newTestReconciler(*f.reconciler, provider)
	_, err := r.ensureRouteGraph(t.Context(), f.route, f.gateway)
	if !changed || !errors.Is(err, errHTTPRouteChanged) {
		t.Fatalf("changed backend passed graph validation: changed=%t, error=%v", changed, err)
	}
	if len(provider.Calls) != 0 {
		t.Fatal("changed backend reached the provider")
	}
}
