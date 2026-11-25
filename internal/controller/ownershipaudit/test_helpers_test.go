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

package ownershipaudit

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

type Config = controller.Config

func testConfig() Config {
	return Config{
		ControllerName:     "example.com/gateway-api-openstack",
		ControllerVersion:  "test",
		OpenStackProjectID: "project-a",
		ClusterID:          "cluster-a",
	}
}

func gatewayBindingAnnotations(cfg Config, _ string) map[string]string {
	return map[string]string{
		cfg.GatewayClusterIDAnnotation(): cfg.ClusterID,
		cfg.GatewayProjectIDAnnotation(): cfg.OpenStackProjectID,
	}
}

func applyRouteBinding(cfg Config, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) {
	if err := controller.SetRouteBinding(cfg, route, controller.RouteBinding{Identity: controller.RouteIdentity(cfg, gateway, route), Lifecycle: controller.RouteAttached}); err != nil {
		panic(err)
	}
}

func routeBindingJSON(cfg Config, identity cloud.Identity, lifecycle controller.RouteLifecycle, cleanupFailure string) string {
	identity.Controller = string(cfg.ControllerName)
	identity.ControllerVersion = cfg.ControllerVersion
	encoded, err := json.Marshal([]controller.RouteBinding{{Identity: identity, Lifecycle: lifecycle, CleanupFailure: cleanupFailure}})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}
