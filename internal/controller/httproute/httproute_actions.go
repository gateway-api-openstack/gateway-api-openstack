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
	"net/http"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// compileRouteAction rejects actions that the current Amphora model cannot emit.
// No filter is supported yet, so report the first unsupported action.
func compileRouteAction(rule gatewayv1.HTTPRouteRule) error {
	if len(rule.Filters) == 0 {
		if len(rule.BackendRefs) == 0 {
			return compileHTTPResponse(http.StatusInternalServerError)
		}
		return nil
	}
	filter := rule.Filters[0]
	switch filter.Type {
	case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
		return compileRequestHeaderModifier(filter.RequestHeaderModifier)
	case gatewayv1.HTTPRouteFilterRequestRedirect:
		return compileRequestRedirect(filter.RequestRedirect)
	default:
		return newRouteBuildError(routeErrorUnsupported, "HTTPRoute filter %s is not supported", filter.Type)
	}
}

func compileRequestHeaderModifier(_ *gatewayv1.HTTPHeaderFilter) error {
	// TODO(octavia): Lower route-scoped header add, set, and remove operations
	// when a released Octavia API and Amphora implementation expose them.
	// Listener insert_headers cannot represent these operations.
	return newRouteBuildError(routeErrorUnsupported, "RequestHeaderModifier requires route-scoped header operations in Octavia")
}

func compileRequestRedirect(redirect *gatewayv1.HTTPRequestRedirectFilter) error {
	if redirect != nil && redirect.Hostname != nil {
		// Static-host redirects have an Octavia primitive. Phase 5 must prove
		// scheme, port, path, query, and precedence before lowering them.
		return newRouteBuildError(routeErrorUnsupported, "RequestRedirect with a fixed hostname is not implemented")
	}
	// TODO(octavia): Lower redirects that preserve the request hostname when
	// the public API supports dynamic redirects with the required semantics.
	return newRouteBuildError(routeErrorUnsupported, "RequestRedirect without hostname requires request-hostname preservation in Octavia")
}

func compileHTTPResponse(statusCode int) error {
	// TODO(octavia): Lower explicit 404 and 500 responses when the public API
	// provides fixed responses and an unmatched-route fallback. L7 REJECT
	// returns 403, and an empty pool returns 503, so neither is a substitute.
	return newRouteBuildError(routeErrorUnsupported, "HTTP %d responses require a fixed-response action in Octavia", statusCode)
}
