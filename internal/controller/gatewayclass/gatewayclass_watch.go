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

package gatewayclass

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

func (r *Reconciler) gatewayClassRequestsForParameters(ctx context.Context, object client.Object) []reconcile.Request {
	if object == nil {
		return nil
	}
	classes := &gatewayv1.GatewayClassList{}
	if err := r.List(ctx, classes, client.MatchingFields{controller.IndexGatewayClassByParameters: object.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "Could not list GatewayClasses for parameters")
		return nil
	}
	keys := make(map[types.NamespacedName]struct{}, len(classes.Items))
	for _, class := range classes.Items {
		if class.Spec.ControllerName == r.Config.ControllerName {
			keys[types.NamespacedName{Name: class.Name}] = struct{}{}
		}
	}
	return controller.SortedRequests(keys)
}
