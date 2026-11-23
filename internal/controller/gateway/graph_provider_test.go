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

package gateway

import (
	"context"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func newTestReconciler(value Reconciler, provider cloud.GraphProvider) *Reconciler {
	r := &value
	if r.TopologyResolver == nil {
		r.TopologyResolver, _ = provider.(cloud.TopologyResolver)
	}
	if r.Writer == nil {
		r.Writer = &graph.Writer{Coordinator: r.Coordinator, Gateway: r, Routes: emptyTestRoutes{}, Provider: provider}
	}
	return r
}

type emptyTestRoutes struct{}

func (emptyTestRoutes) ObserveRouteGraph(context.Context, graph.Request, *gatewayv1.Gateway) (graph.RouteInputs, error) {
	return graph.RouteInputs{}, nil
}
