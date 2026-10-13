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
	"strings"
	"testing"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestUnknownProviderFailuresAreRedacted(t *testing.T) {
	reconciler := &Reconciler{}
	cause := errors.New("response=private-body token=secret-token")
	for name, handle := range map[string]func() (ctrl.Result, error){
		"ensure": func() (ctrl.Result, error) {
			return reconciler.handleRouteProviderFailure(nil, gatewayv1.ParentReference{}, false, cause, "EnsureHTTPRoute")
		},
		"delete": func() (ctrl.Result, error) {
			return reconciler.handleRouteFinalizationFailure(context.Background(), nil, cloud.Identity{}, cause)
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := handle()
			if result != (ctrl.Result{}) || !errors.Is(err, cause) || strings.Contains(err.Error(), "private-body") || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("result=%#v error=%v", result, err)
			}
		})
	}
}
