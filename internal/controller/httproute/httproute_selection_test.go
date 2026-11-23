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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestRouteSelectionAuthorizesCrossNamespaceServiceBeforeRead(t *testing.T) {
	for _, granted := range []bool{false, true} {
		name := "denied"
		if granted {
			name = "granted"
		}
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			gateway := programmedTestGateway(cfg)
			parent := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name)}
			older := mapperTestHTTPRoute(gateway.Namespace, "older", parent, "backend")
			older.UID = "older-route"
			older.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
			older.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace("shared"))
			current := mapperTestHTTPRoute(gateway.Namespace, "current", parent, "local")
			current.UID = "current-route"
			current.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
			// This unsupported profile must not be inspected without a grant.
			service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shared", Name: "backend"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}
			objects := []client.Object{older, current, service}
			if granted {
				objects = append(objects, &gatewayv1.ReferenceGrant{
					ObjectMeta: metav1.ObjectMeta{Namespace: "shared", Name: "allow"},
					Spec: gatewayv1.ReferenceGrantSpec{
						From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: gatewayv1.Namespace(gateway.Namespace)}},
						To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1.ObjectName("backend"))}},
					},
				})
			}
			reader := &routeSelectionReader{Reader: indexedFakeClientBuilder(testScheme(t), cfg).WithObjects(objects...).Build(), allowService: granted}
			r := &Reconciler{Config: cfg}
			selected, err := r.isSelectedRouteWithReader(t.Context(), reader, current, managedRouteParent{ref: parent, gateway: gateway}, true)
			if err != nil || selected != granted {
				t.Fatalf("selected=%t, error=%v, want %t", selected, err, granted)
			}
			expectedReads := 0
			if granted {
				expectedReads = 1
			}
			if reader.serviceReads != expectedReads {
				t.Fatalf("Service reads=%d, want %d", reader.serviceReads, expectedReads)
			}
		})
	}
}

type routeSelectionReader struct {
	client.Reader
	allowService bool
	serviceReads int
}

func (r *routeSelectionReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Service); ok {
		r.serviceReads++
		if !r.allowService {
			return errors.New("Service read preceded ReferenceGrant authorization")
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}
