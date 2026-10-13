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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// nodeEventInputs reuses dependency reads while mapping one Node event.
// It is discarded after enqueueing and never authorizes a graph mutation.
type nodeEventInputs struct {
	parents   map[string]nodeEligibilityObservation
	services  map[types.NamespacedName]*nodeServiceObservation
	endpoints map[types.NamespacedName]nodeEligibilityObservation
}

type nodeEligibilityObservation struct {
	eligible bool
	err      error
}

type nodeServiceObservation struct {
	service corev1.Service
	err     error
}

func (inputs *nodeEventInputs) hasManagedParent(ctx context.Context, r *Reconciler, route *gatewayv1.HTTPRoute) (bool, error) {
	keys := controller.ParentGatewayKeys(route)
	if len(keys) != 1 {
		return false, nil
	}
	if observation, ok := inputs.parents[keys[0]]; ok {
		return observation.eligible, observation.err
	}
	parents, err := r.managedParents(ctx, route)
	observation := nodeEligibilityObservation{eligible: len(parents) > 0, err: err}
	inputs.parents[keys[0]] = observation
	return observation.eligible, observation.err
}

func (inputs *nodeEventInputs) service(ctx context.Context, reader client.Reader, key types.NamespacedName) (*corev1.Service, error) {
	observation, ok := inputs.services[key]
	if !ok {
		observation = &nodeServiceObservation{}
		observation.err = reader.Get(ctx, key, &observation.service)
		inputs.services[key] = observation
	}
	return &observation.service, observation.err
}

func (inputs *nodeEventInputs) hasReadyEndpoint(ctx context.Context, r *Reconciler, key types.NamespacedName, nodeName string) (bool, error) {
	if observation, ok := inputs.endpoints[key]; ok {
		return observation.eligible, observation.err
	}
	ready, err := r.nodeHasReadyEndpoint(ctx, key, nodeName)
	inputs.endpoints[key] = nodeEligibilityObservation{eligible: ready, err: err}
	return ready, err
}
