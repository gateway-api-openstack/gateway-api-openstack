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
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	gatewaycontroller "github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/gateway"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

func newTestReconciler(value Reconciler, provider cloud.GraphProvider) *Reconciler {
	r := &value
	gateway := &gatewaycontroller.Reconciler{Client: r.Client, APIReader: r.APIReader, Config: r.Config, Coordinator: r.Coordinator}
	if r.Writer == nil {
		r.Writer = &graph.Writer{Coordinator: r.Coordinator, Gateway: gateway, Routes: r, Provider: provider}
	}
	return r
}

func newTestGatewayReconciler(value gatewaycontroller.Reconciler, provider cloud.GraphProvider) *gatewaycontroller.Reconciler {
	r := &value
	if r.TopologyResolver == nil {
		r.TopologyResolver, _ = provider.(cloud.TopologyResolver)
	}
	routes := &Reconciler{Client: r.Client, APIReader: r.APIReader, Config: r.Config, Coordinator: r.Coordinator}
	writer := &graph.Writer{Coordinator: r.Coordinator, Gateway: r, Routes: routes, Provider: provider}
	r.Writer, routes.Writer = writer, writer
	return r
}
