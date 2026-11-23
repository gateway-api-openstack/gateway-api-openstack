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

package controller

// RouteLifecycle identifies durable route fragment intent.
type RouteLifecycle string

const (
	// RouteAttached identifies a durably bound desired route fragment.
	RouteAttached RouteLifecycle = "Attached"
	// RouteDetaching records cleanup authority that survives spec changes and restart.
	RouteDetaching RouteLifecycle = "Detaching"
)

// RouteBindingsAnnotation identifies the controller-owned per-Gateway binding list.
func (c Config) RouteBindingsAnnotation() string {
	return c.Domain() + "/route-bindings-" + c.controllerKey()
}
