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
	"context"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

type gatewaySourceFunc func(context.Context, Request) (GatewayInput, error)

func (f gatewaySourceFunc) ObserveGatewayGraph(ctx context.Context, request Request) (GatewayInput, error) {
	return f(ctx, request)
}

type routeSourceFunc func(context.Context, Request, *gatewayv1.Gateway) (RouteInputs, error)

func (f routeSourceFunc) ObserveRouteGraph(ctx context.Context, request Request, gateway *gatewayv1.Gateway) (RouteInputs, error) {
	return f(ctx, request, gateway)
}

type graphProviderFunc func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error)

func (f graphProviderFunc) ReconcileGatewayGraph(ctx context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
	return f(ctx, spec)
}

func writerRequest() Request {
	return Request{Action: Ensure, Identity: cloud.Identity{
		OpenStackProjectID: "project-a", ClusterID: "cluster-a", Controller: "example.com/gateway-controller",
		GatewayNamespace: "default", GatewayName: "edge", GatewayUID: "gateway-uid",
	}}
}

func writerFixture() *Writer {
	return &Writer{
		Coordinator: &Coordinator{},
		Gateway: gatewaySourceFunc(func(_ context.Context, request Request) (GatewayInput, error) {
			return GatewayInput{
				Object: &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{UID: types.UID(request.Identity.GatewayUID)}},
				Node:   cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure, Spec: cloud.GatewaySpec{Identity: request.Identity}},
			}, nil
		}),
		Routes: routeSourceFunc(func(context.Context, Request, *gatewayv1.Gateway) (RouteInputs, error) { return RouteInputs{}, nil }),
		Provider: graphProviderFunc(func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
			return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome()}, nil
		}),
	}
}

func TestWriterObservesFreshInputsAfterAcquiringGateway(t *testing.T) {
	w := writerFixture()
	request := writerRequest()
	var generation atomic.Int64
	generation.Store(1)
	entered, continueFirst := make(chan struct{}), make(chan struct{})
	ports := make(chan int, 2)
	w.Gateway = gatewaySourceFunc(func(context.Context, Request) (GatewayInput, error) {
		current := generation.Load()
		return GatewayInput{
			Object: &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Generation: current}},
			Node:   cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure, Spec: cloud.GatewaySpec{ListenerPort: int(80 + current)}},
		}, nil
	})
	w.Routes = routeSourceFunc(func(_ context.Context, _ Request, gateway *gatewayv1.Gateway) (RouteInputs, error) {
		if gateway.Generation != generation.Load() {
			t.Error("route source received a stale Gateway observation")
		}
		return RouteInputs{}, nil
	})
	w.Provider = graphProviderFunc(func(_ context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
		ports <- spec.Gateway.Spec.ListenerPort
		if spec.Gateway.Spec.ListenerPort == 81 {
			close(entered)
			<-continueFirst
		}
		return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome()}, nil
	})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := w.ReconcileGraph(context.Background(), request); first <- err }()
	<-entered
	go func() { _, err := w.ReconcileGraph(context.Background(), request); second <- err }()
	waitForGraphReferences(t, w.Coordinator, types.UID(request.Identity.GatewayUID), 2)
	generation.Store(2)
	close(continueFirst)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if firstPort, secondPort := <-ports, <-ports; firstPort != 81 || secondPort != 82 {
		t.Fatalf("provider listener ports = %d, %d, want observations before and after lock wait", firstPort, secondPort)
	}
	waitForGraphReferences(t, w.Coordinator, types.UID(request.Identity.GatewayUID), 0)
}

func TestWriterDifferentGatewayDoesNotWait(t *testing.T) {
	w := writerFixture()
	request := writerRequest()
	release, err := Acquire(context.Background(), w.Coordinator, request.Identity.GatewayUID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	request.Identity.GatewayUID = "other-gateway-uid"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := w.ReconcileGraph(ctx, request); err != nil {
		t.Fatalf("independent Gateway was blocked: %v", err)
	}
}

func TestWriterCanceledWaitDoesNotObserveOrCallProvider(t *testing.T) {
	w := writerFixture()
	request := writerRequest()
	release, err := Acquire(context.Background(), w.Coordinator, request.Identity.GatewayUID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var reads, providerCalls atomic.Int32
	w.Gateway = gatewaySourceFunc(func(context.Context, Request) (GatewayInput, error) { reads.Add(1); return GatewayInput{}, nil })
	w.Provider = graphProviderFunc(func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
		providerCalls.Add(1)
		return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome()}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() { _, err := w.ReconcileGraph(ctx, request); completed <- err }()
	waitForGraphReferences(t, w.Coordinator, types.UID(request.Identity.GatewayUID), 2)
	cancel()
	if err := <-completed; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if reads.Load() != 0 || providerCalls.Load() != 0 {
		t.Fatal("canceled lock waiter performed graph work")
	}
}

func TestWriterFailedSourceOrFencePreventsProviderCall(t *testing.T) {
	for _, stage := range []string{"gateway source", "route source", "route fence", "gateway fence"} {
		t.Run(stage, func(t *testing.T) {
			w := writerFixture()
			failure := errors.New("input changed")
			var calls []string
			failAt := func(current string) error {
				calls = append(calls, current)
				if current == stage {
					return failure
				}
				return nil
			}
			w.Gateway = gatewaySourceFunc(func(context.Context, Request) (GatewayInput, error) {
				return GatewayInput{Validate: func(context.Context) error { return failAt("gateway fence") }}, failAt("gateway source")
			})
			w.Routes = routeSourceFunc(func(context.Context, Request, *gatewayv1.Gateway) (RouteInputs, error) {
				return RouteInputs{Validate: func(context.Context) error { return failAt("route fence") }}, failAt("route source")
			})
			w.Provider = graphProviderFunc(func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
				t.Error("failed input allowed provider call")
				return cloud.GatewayGraphResult{}, nil
			})
			if _, err := w.ReconcileGraph(context.Background(), writerRequest()); !errors.Is(err, failure) {
				t.Fatalf("error = %v, want source failure", err)
			}
			order := []string{"gateway source", "route source", "route fence", "gateway fence"}
			want := order[:slices.Index(order, stage)+1]
			if !slices.Equal(calls, want) {
				t.Fatalf("calls = %v, want prerequisite order %v", calls, want)
			}
			waitForGraphReferences(t, w.Coordinator, types.UID(writerRequest().Identity.GatewayUID), 0)
		})
	}
}

func TestWriterReconstructsCompleteGraphAfterRestart(t *testing.T) {
	request := writerRequest()
	request.Identity.RouteNamespace, request.Identity.RouteName, request.Identity.RouteUID = "default", "route", "route-uid"
	var reads, providerCalls, mutations int
	var observed cloud.GatewayGraphSpec
	provider := graphProviderFunc(func(_ context.Context, spec cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
		providerCalls++
		if !reflect.DeepEqual(spec, observed) {
			mutations++
			observed = spec
		}
		return cloud.GatewayGraphResult{Outcome: cloud.ReadyOutcome()}, nil
	})
	for range 2 {
		w := writerFixture()
		w.Provider = provider
		w.Routes = routeSourceFunc(func(context.Context, Request, *gatewayv1.Gateway) (RouteInputs, error) {
			reads++
			return RouteInputs{Nodes: []cloud.RouteGraphNode{
				{Disposition: cloud.GraphEnsure, Spec: cloud.RouteSpec{Identity: request.Identity}},
			}}, nil
		})
		if _, err := w.ReconcileGraph(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 || providerCalls != 2 || mutations != 1 {
		t.Fatalf("reads = %d, provider calls = %d, mutations = %d", reads, providerCalls, mutations)
	}
	if observed.Identity.RouteUID != "" || len(observed.Routes) != 1 || observed.Routes[0].Spec.Identity != request.Identity {
		t.Fatalf("writer confused whole-graph and route identity: %#v", observed)
	}
}

func TestWriterPreservesComponentResults(t *testing.T) {
	w := writerFixture()
	want := cloud.GatewayGraphResult{
		Outcome: cloud.ProgressingOutcome("members changing", time.Second),
		Gateway: cloud.GatewayReadyResult(cloud.GatewayState{LoadBalancerID: "lb-1"}),
		Routes:  []cloud.RouteGraphResult{{Identity: writerRequest().Identity, Result: cloud.RouteProgressingResult("waiting", time.Second)}},
	}
	w.Provider = graphProviderFunc(func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) { return want, nil })
	got, err := w.ReconcileGraph(context.Background(), writerRequest())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, error = %v, want independent component outcomes %#v", got, err, want)
	}
	w.Provider = graphProviderFunc(func(context.Context, cloud.GatewayGraphSpec) (cloud.GatewayGraphResult, error) {
		return cloud.GatewayGraphResult{}, nil
	})
	if _, err := w.ReconcileGraph(context.Background(), writerRequest()); err == nil {
		t.Fatal("unknown provider outcome was accepted")
	}
}
