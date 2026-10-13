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

package graph

import (
	"fmt"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

// These benchmarks isolate pure model and diff cost. They perform no Kubernetes
// or OpenStack requests and make no assertion about end-to-end cloud latency.
func BenchmarkGatewayGraphCalculation(b *testing.B) {
	for _, count := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("members=%d", count), func(b *testing.B) {
			provider, spec, actual := benchmarkGatewayGraph(b, count)
			b.Run("model", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := provider.buildDesiredGatewayGraph(spec); err != nil {
						b.Fatal(err)
					}
				}
			})
			desired, err := provider.buildDesiredGatewayGraph(spec)
			if err != nil {
				b.Fatal(err)
			}
			b.Run("diff", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					plan, result, err := provider.buildGatewayGraphPlan(desired, actual)
					if err != nil || len(plan) != 0 || result.Outcome.State != cloud.OutcomeReady {
						b.Fatalf("unexpected diff: %d operations, %v", len(plan), err)
					}
				}
			})
		})
	}
}

func benchmarkGatewayGraph(b *testing.B, count int) (*Provider, cloud.GatewayGraphSpec, observedGatewayGraph) {
	b.Helper()
	identity, err := NewIdentity(cloud.Identity{OpenStackProjectID: "project-a", ClusterID: "cluster", Controller: "example.test/controller",
		GatewayNamespace: "example", GatewayName: "edge", GatewayUID: "gateway-uid", RouteNamespace: "example", RouteName: "route", RouteUID: "route-uid"})
	if err != nil {
		b.Fatal(err)
	}
	spec := completeGraphSpec(identity)
	spec.Routes[0].Spec.Hostname = ""
	spec.Routes[0].Spec.Members = nil
	_, route := convergedRoutePlanFixture()
	route.members = nil
	for index := range count {
		address := fmt.Sprintf("10.%d.%d.%d", index/(254*254), (index/254)%254, index%254+1)
		spec.Routes[0].Spec.Members = append(spec.Routes[0].Spec.Members, cloud.Member{Address: address, Port: 30080})
		route.members = append(route.members, pools.Member{ID: fmt.Sprintf("member-%d", index), Address: address, ProtocolPort: 30080, SubnetID: "member-subnet", AdminStateUp: true})
	}
	actual := observedGatewayGraph{
		loadBalancer: &loadbalancers.LoadBalancer{ID: "load-balancer-1", Provider: "amphora", VipSubnetID: "vip-subnet", AdminStateUp: true},
		listener:     &listeners.Listener{ID: "listener-1", ProtocolPort: 80, AdminStateUp: true},
		state:        spec.Routes[0].Spec.Gateway, routes: []observedRouteGraph{route},
	}
	return &Provider{projectID: "project-a"}, spec, actual
}
