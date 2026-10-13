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
	"fmt"
	"time"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

// Action identifies the durable lifecycle intent that the sources must verify.
type Action string

const (
	// Ensure reconciles the complete desired Gateway graph.
	Ensure Action = "Ensure"
	// DeleteGateway removes a graph after validating Gateway cleanup authority.
	DeleteGateway Action = "DeleteGateway"
	// DeleteRoute removes a durably detaching route and preserves its Gateway.
	DeleteRoute Action = "DeleteRoute"
)

// Request carries the identity and Kubernetes revision that triggered a write.
// It is not mutation authority until the sources validate fresh objects.
type Request struct {
	Identity cloud.Identity
	Action   Action
	Gateway  *gatewayv1.Gateway
	Route    *gatewayv1.HTTPRoute
}

// Reconciler is the sole Kubernetes-facing graph mutation boundary.
type Reconciler interface {
	ReconcileGraph(context.Context, Request) (cloud.GatewayGraphResult, error)
}

// GatewayInput contains one request-local observation and its mutation fence.
type GatewayInput struct {
	Object   *gatewayv1.Gateway
	Node     cloud.GatewayGraphNode
	Validate func(context.Context) error
}

// RouteInputs contains the complete route set and its mutation fence.
type RouteInputs struct {
	Nodes    []cloud.RouteGraphNode
	Validate func(context.Context) error
}

// GatewaySource observes and validates the Gateway portion of a graph.
type GatewaySource interface {
	ObserveGatewayGraph(context.Context, Request) (GatewayInput, error)
}

// RouteSource observes a complete live set of bound route fragments.
type RouteSource interface {
	ObserveRouteGraph(context.Context, Request, *gatewayv1.Gateway) (RouteInputs, error)
}

// Writer serializes fresh graph construction and the single provider operation.
// All fields are immutable process dependencies.
type Writer struct {
	Coordinator *Coordinator
	Gateway     GatewaySource
	Routes      RouteSource
	Provider    cloud.GraphProvider
	Observer    StageObserver
}

// ReconcileGraph rebuilds the graph under its UID lock on every call.
func (w *Writer) ReconcileGraph(ctx context.Context, request Request) (cloud.GatewayGraphResult, error) {
	if w == nil || w.Gateway == nil || w.Routes == nil || w.Provider == nil {
		return cloud.GatewayGraphResult{}, errors.New("gateway graph writer dependencies are required")
	}
	if err := cloud.ValidateGatewayIdentity(request.Identity); err != nil {
		return cloud.GatewayGraphResult{}, err
	}
	started := time.Now()
	release, err := Acquire(ctx, w.Coordinator, request.Identity.GatewayUID)
	w.observeStage(StageLockWait, started, err)
	if err != nil {
		return cloud.GatewayGraphResult{}, fmt.Errorf("acquire Gateway graph: %w", err)
	}
	defer release()
	started = time.Now()
	gateway, err := w.Gateway.ObserveGatewayGraph(ctx, request)
	w.observeStage(StageGatewayInput, started, err)
	if err != nil {
		return cloud.GatewayGraphResult{}, err
	}
	started = time.Now()
	routes, err := w.Routes.ObserveRouteGraph(ctx, request, gateway.Object)
	w.observeStage(StageRouteInput, started, err)
	if err != nil {
		return cloud.GatewayGraphResult{}, err
	}
	started = time.Now()
	err = validateInputs(ctx, gateway, routes)
	w.observeStage(StageValidation, started, err)
	if err != nil {
		return cloud.GatewayGraphResult{}, err
	}
	identity := request.Identity
	identity.RouteNamespace, identity.RouteName, identity.RouteUID = "", "", ""
	started = time.Now()
	result, err := w.Provider.ReconcileGatewayGraph(ctx, cloud.GatewayGraphSpec{Identity: identity, Gateway: gateway.Node, Routes: routes.Nodes})
	if err == nil {
		if outcomeErr := result.Outcome.Validate(); outcomeErr != nil {
			err = fmt.Errorf("validate Gateway graph outcome: %w", outcomeErr)
		}
	}
	w.observeStage(StageProvider, started, err)
	return result, err
}

func validateInputs(ctx context.Context, gateway GatewayInput, routes RouteInputs) error {
	if routes.Validate != nil {
		if err := routes.Validate(ctx); err != nil {
			return err
		}
	}
	if gateway.Validate != nil {
		return gateway.Validate(ctx)
	}
	return nil
}

func (w *Writer) observeStage(stage Stage, started time.Time, err error) {
	if w.Observer != nil {
		w.Observer.ObserveGraphStage(stage, time.Since(started), err != nil)
	}
}
