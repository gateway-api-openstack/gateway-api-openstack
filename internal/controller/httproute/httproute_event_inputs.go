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

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
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

func (inputs *nodeEventInputs) hasNodePortParent(ctx context.Context, r *Reconciler, route *gatewayv1.HTTPRoute) (bool, error) {
	var readErr error
	for _, parent := range route.Spec.ParentRefs {
		if !controller.IsGatewayParentRef(parent) {
			continue
		}
		key := types.NamespacedName{Namespace: route.Namespace, Name: string(parent.Name)}
		if parent.Namespace != nil {
			key.Namespace = string(*parent.Namespace)
		}
		indexKey := controller.ObjectKeyString(key)
		observation, found := inputs.parents[indexKey]
		if !found {
			observation.eligible, observation.err = observeNodePortGateway(ctx, r, key)
			inputs.parents[indexKey] = observation
		}
		if observation.eligible {
			return true, nil
		}
		readErr = errors.Join(readErr, observation.err)
	}
	return false, readErr
}

func observeNodePortGateway(ctx context.Context, r *Reconciler, key types.NamespacedName) (bool, error) {
	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, key, &gateway); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	var class gatewayv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if class.Spec.ControllerName != r.Config.ControllerName || !controller.HasGatewayTopologyBinding(r.Config, &gateway) {
		return false, nil
	}
	topology, err := controller.StoredGatewayTopology(r.Config, &gateway)
	return err == nil && topology.Topology.MemberMode == cloud.MemberModeNodePort, err
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
