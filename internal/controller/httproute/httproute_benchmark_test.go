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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func BenchmarkNodePortModel(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		for _, policy := range []corev1.ServiceExternalTrafficPolicy{corev1.ServiceExternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyLocal} {
			b.Run(fmt.Sprintf("%s/nodes=%d", policy, count), func(b *testing.B) {
				service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "backend"}, Spec: corev1.ServiceSpec{ExternalTrafficPolicy: policy}}
				nodes, endpoints := memberModelFixture(count)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					localNodes, err := readyBackendNodes(service, &corev1.ServicePort{}, endpoints)
					if err != nil {
						b.Fatal(err)
					}
					members, err := nodePortMembersForNodes(service, 30080, corev1.NodeInternalIP, localNodes, nodes)
					if err != nil || len(members) != count {
						b.Fatalf("members = %d, error = %v", len(members), err)
					}
				}
			})
		}
	}
}

func memberModelFixture(count int) ([]corev1.Node, []discoveryv1.EndpointSlice) {
	nodes := make([]corev1.Node, 0, count)
	endpoints := make([]discoveryv1.Endpoint, 0, count)
	for i := range count {
		name := fmt.Sprintf("worker-%04d", count-i)
		nodes = append(nodes, corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{
				Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: fmt.Sprintf("10.%d.%d.%d", i/65536, i/256%256, i%256)}},
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		})
		endpoints = append(endpoints, discoveryv1.Endpoint{NodeName: ptr.To(name), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
	}
	return nodes, []discoveryv1.EndpointSlice{{AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Port: ptr.To(int32(80))}}, Endpoints: endpoints}}
}

type fanoutCountingClient struct {
	client.Client
	gets, lists int
}

func (c *fanoutCountingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	c.gets++
	return c.Client.Get(ctx, key, object, options...)
}

func (c *fanoutCountingClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	c.lists++
	return c.Client.List(ctx, list, options...)
}

func nodeFanoutFixture(t testing.TB, count int, policy corev1.ServiceExternalTrafficPolicy) (*Reconciler, *fanoutCountingClient, *corev1.Node) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, discoveryv1.AddToScheme, gatewayv1.Install} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	service, endpoints, node := validNodePortBackend("default", "backend")
	service.Spec.ExternalTrafficPolicy = policy
	objects := []client.Object{
		service, endpoints, node,
		&gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "managed"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: cfg.ControllerName}},
		&gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "foreign"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "other.example.com/controller"}},
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "edge"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "managed"}},
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "foreign"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "foreign"}},
	}
	for i := range count {
		objects = append(objects,
			mapperTestHTTPRoute("default", fmt.Sprintf("route-%04d", i), gatewayv1.ParentReference{Name: "edge"}, "backend"),
			mapperTestHTTPRoute("default", fmt.Sprintf("foreign-%04d", i), gatewayv1.ParentReference{Name: "foreign"}, "backend"),
		)
	}
	base := rawIndexedFakeClientBuilder(scheme, cfg).WithObjects(objects...).Build()
	counted := &fanoutCountingClient{Client: base}
	return &Reconciler{Client: counted, Config: cfg}, counted, node
}

// BenchmarkNodeEventFanout measures cached-reader work and enqueue fan-out only.
// It does not call OpenStack or run a controller against a live cluster.
func BenchmarkNodeEventFanout(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		for _, policy := range []corev1.ServiceExternalTrafficPolicy{corev1.ServiceExternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyLocal} {
			b.Run(fmt.Sprintf("%s/routes=%d", policy, count), func(b *testing.B) {
				r, reader, node := nodeFanoutFixture(b, count, policy)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					requests := r.enqueueHTTPRoutesForNode(context.Background(), node)
					if len(requests) != count {
						b.Fatalf("enqueues = %d, want %d managed routes", len(requests), count)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(reader.gets)/float64(b.N), "gets/event")
				b.ReportMetric(float64(reader.lists)/float64(b.N), "lists/event")
				b.ReportMetric(float64(count), "enqueues/event")
			})
		}
	}
}

// BenchmarkEndpointSliceEventFanout measures one mapper invocation. Update
// handlers invoke the mapper for both old and new objects before deduplication.
func BenchmarkEndpointSliceEventFanout(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("routes=%d", count), func(b *testing.B) {
			r, reader, _ := nodeFanoutFixture(b, count, corev1.ServiceExternalTrafficPolicyLocal)
			endpoint := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "backend"},
			}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				// Service events retain direct routes for both controllers. Their
				// reconciler checks ownership before any write or provider call.
				if got := len(r.enqueueHTTPRoutesForEndpointSlice(context.Background(), endpoint)); got != 2*count {
					b.Fatalf("enqueues = %d, want %d referencing routes", got, 2*count)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(reader.gets)/float64(b.N), "gets/event")
			b.ReportMetric(float64(reader.lists)/float64(b.N), "lists/event")
			b.ReportMetric(float64(2*count), "enqueues/event")
		})
	}
}

func TestNodeEventReadsSharedDependencies(t *testing.T) {
	const routes, events = 20, 5
	r, reader, node := nodeFanoutFixture(t, routes, corev1.ServiceExternalTrafficPolicyLocal)
	for range events {
		requests := r.enqueueHTTPRoutesForNode(context.Background(), node)
		if len(requests) != routes {
			t.Fatalf("enqueued requests = %d, want %d managed routes", len(requests), routes)
		}
	}
	// Each event resolves two parents and one Service once, with one route
	// list and one EndpointSlice list, regardless of the number of routes.
	if reader.gets > events*5 || reader.lists > events*2 {
		t.Fatalf("event burst amplified reads: %d gets, %d lists", reader.gets, reader.lists)
	}
}

func TestEndpointSliceEventReadsEachSharedParentOnce(t *testing.T) {
	for _, count := range []int{1, 20, 100} {
		t.Run(fmt.Sprintf("routes=%d", count), func(t *testing.T) {
			r, reader, _ := nodeFanoutFixture(t, count, corev1.ServiceExternalTrafficPolicyLocal)
			endpoint := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "backend"},
			}}
			for event := range 2 {
				requests := r.enqueueHTTPRoutesForEndpointSlice(context.Background(), endpoint)
				if len(requests) != 2*count || reader.gets != 0 || reader.lists != (event+1)*3 {
					t.Fatalf("event %d: enqueues=%d, gets=%d, lists=%d, want %d/0/%d", event, len(requests), reader.gets, reader.lists, 2*count, (event+1)*3)
				}
			}
		})
	}
}

func TestMemberModelPreservesInputOrderAndDeduplicatesAddresses(t *testing.T) {
	nodes, endpoints := memberModelFixture(2)
	nodes[0].Status.Addresses[0].Address = "10.0.0.1"
	nodes[1].Status.Addresses[0].Address = "10.0.0.0"
	nodes = append(nodes, *nodes[0].DeepCopy())
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "backend"}}
	localNodes, err := readyBackendNodes(service, &corev1.ServicePort{}, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	before := nodes[0].Name
	members, err := nodePortMembersForNodes(service, 30080, corev1.NodeInternalIP, localNodes, nodes)
	if err != nil || len(members) != 2 || nodes[0].Name != before {
		t.Fatalf("members = %#v, error = %v, first input = %s", members, err, nodes[0].Name)
	}
	if members[0] != (cloud.Member{Address: "10.0.0.0", Port: 30080}) || members[1] != (cloud.Member{Address: "10.0.0.1", Port: 30080}) {
		t.Fatalf("members are not deterministic: %#v", members)
	}
}
