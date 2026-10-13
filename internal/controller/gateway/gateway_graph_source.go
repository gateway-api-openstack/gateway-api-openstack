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
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/graph"
)

// ObserveGatewayGraph validates the live Gateway component under the writer lock.
func (r *Reconciler) ObserveGatewayGraph(ctx context.Context, request graph.Request) (graph.GatewayInput, error) {
	if r.APIReader == nil {
		return graph.GatewayInput{}, controller.ErrAPIReaderRequired
	}
	if request.Action == graph.DeleteRoute {
		var current gatewayv1.Gateway
		err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: request.Identity.GatewayNamespace, Name: request.Identity.GatewayName}, &current)
		if err != nil && !apierrors.IsNotFound(err) {
			return graph.GatewayInput{}, err
		}
		input := graph.GatewayInput{Node: cloud.GatewayGraphNode{Disposition: cloud.GraphPreserve}}
		if err == nil && string(current.UID) == request.Identity.GatewayUID {
			input.Object = &current
		}
		return input, nil
	}
	if request.Gateway == nil {
		return graph.GatewayInput{}, errors.New("expected Gateway revision is required")
	}
	if request.Action == graph.DeleteGateway {
		snapshot, err := r.observeGatewayCleanupSnapshot(ctx, request.Gateway)
		if err != nil {
			return graph.GatewayInput{}, err
		}
		if snapshot.disposition == gatewayCleanupHold || snapshot.identity != request.Identity {
			return graph.GatewayInput{}, errGatewayChanged
		}
		return graph.GatewayInput{
			Object: request.Gateway.DeepCopy(), Node: cloud.GatewayGraphNode{Disposition: cloud.GraphDelete},
			Validate: func(ctx context.Context) error {
				latest, err := r.observeGatewayCleanupSnapshot(ctx, request.Gateway)
				if err != nil {
					return err
				}
				if *latest != *snapshot {
					return errGatewayChanged
				}
				return nil
			},
		}, nil
	}
	if request.Action != graph.Ensure {
		return graph.GatewayInput{}, errors.New("invalid Gateway graph action")
	}
	snapshot, err := r.observeGatewayMutationSnapshot(ctx, request.Gateway, gatewayBindingRequired, "")
	if err != nil {
		return graph.GatewayInput{}, err
	}
	desired := r.gatewaySpec(snapshot.gateway, snapshot.listener, snapshot.topology)
	return graph.GatewayInput{
		Object: snapshot.gateway, Node: cloud.GatewayGraphNode{Disposition: cloud.GraphEnsure, Spec: desired},
		Validate: func(ctx context.Context) error { return r.sealGatewayMutationSnapshot(ctx, snapshot) },
	}, nil
}
