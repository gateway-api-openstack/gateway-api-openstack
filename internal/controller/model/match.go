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

package model

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// NormalizeMatch applies Gateway API defaults and normalizes Core HTTP matches.
// Unsupported predicates return a diagnostic rather than widening the match.
func NormalizeMatch(input gatewayv1.HTTPRouteMatch) (Match, error) {
	result := Match{PathType: gatewayv1.PathMatchPathPrefix, PathValue: "/"}
	if input.Method != nil {
		switch *input.Method {
		case gatewayv1.HTTPMethodGet, gatewayv1.HTTPMethodHead, gatewayv1.HTTPMethodPost, gatewayv1.HTTPMethodPut,
			gatewayv1.HTTPMethodDelete, gatewayv1.HTTPMethodConnect, gatewayv1.HTTPMethodOptions, gatewayv1.HTTPMethodTrace, gatewayv1.HTTPMethodPatch:
		default:
			return Match{}, unsupported(fmt.Sprintf("unknown HTTP method %q", *input.Method), ScopeRoute)
		}
	}
	for _, query := range input.QueryParams {
		if query.Type != nil && *query.Type != gatewayv1.QueryParamMatchExact && *query.Type != gatewayv1.QueryParamMatchRegularExpression {
			return Match{}, unsupported(fmt.Sprintf("unknown query match type %q", *query.Type), ScopeRoute)
		}
	}
	if input.Path != nil {
		if input.Path.Type != nil {
			result.PathType = *input.Path.Type
		}
		if input.Path.Value != nil {
			result.PathValue = *input.Path.Value
		}
	}
	switch result.PathType {
	case gatewayv1.PathMatchExact, gatewayv1.PathMatchPathPrefix:
	case gatewayv1.PathMatchRegularExpression:
		return Match{}, unsupported("regular expression paths are not implemented", ScopeRule)
	default:
		return Match{}, unsupported(fmt.Sprintf("unknown path match type %q", result.PathType), ScopeRoute)
	}
	if !validPath(result.PathValue) {
		return Match{}, unsupported(fmt.Sprintf("path %q is not a valid absolute Gateway API path", result.PathValue), ScopeRule)
	}
	if result.PathType == gatewayv1.PathMatchPathPrefix && len(result.PathValue) > 1 {
		result.PathValue = strings.TrimSuffix(result.PathValue, "/")
	}
	seen := make(map[string]bool, len(input.Headers))
	for _, header := range input.Headers {
		name := strings.ToLower(string(header.Name))
		// Gateway API uses the first case-equivalent header entry.
		if seen[name] {
			continue
		}
		seen[name] = true
		if header.Type != nil && *header.Type != gatewayv1.HeaderMatchExact {
			scope := ScopeRoute
			if *header.Type == gatewayv1.HeaderMatchRegularExpression {
				scope = ScopeRule
			}
			return Match{}, unsupported(fmt.Sprintf("header match type %q is not implemented", *header.Type), scope)
		}
		result.Headers = append(result.Headers, HeaderMatch{Name: name, Value: header.Value})
	}
	slices.SortFunc(result.Headers, func(a, b HeaderMatch) int { return cmp.Compare(a.Name, b.Name) })
	if input.Method != nil || len(input.QueryParams) != 0 {
		return Match{}, unsupported("method and query parameter matching is not implemented", ScopeRule)
	}
	return result, nil
}

func validPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "//") || strings.Contains(path, "/./") || strings.Contains(path, "/../") ||
		strings.Contains(strings.ToLower(path), "%2f") || strings.Contains(path, "#") || strings.HasSuffix(path, "/.") || strings.HasSuffix(path, "/..") {
		return false
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-/_~.!$&'()*+,;=:@", rune(c)) {
			continue
		}
		if c != '%' || i+2 >= len(path) || !hex(path[i+1]) || !hex(path[i+2]) {
			return false
		}
		i += 2
	}
	return true
}

func hex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unsupported(message string, scope DiagnosticScope) *Diagnostic {
	return &Diagnostic{Condition: string(gatewayv1.RouteConditionAccepted), Reason: string(gatewayv1.RouteReasonUnsupportedValue), Message: message, Scope: scope}
}

// compareMatches follows the pinned Core hostname, path, header, and route
// precedence. Unsupported method and query predicates never enter this order.
func compareMatches(a, b RouteMatch) int {
	aExact, aLength := hostnameSpecificity(a.Hostname)
	bExact, bLength := hostnameSpecificity(b.Hostname)
	if order := cmp.Compare(bExact, aExact); order != 0 {
		return order
	}
	if order := cmp.Compare(bLength, aLength); order != 0 {
		return order
	}
	if a.Match.PathType != b.Match.PathType {
		if a.Match.PathType == gatewayv1.PathMatchExact {
			return -1
		}
		return 1
	}
	if a.Match.PathType == gatewayv1.PathMatchPathPrefix {
		if order := cmp.Compare(len(b.Match.PathValue), len(a.Match.PathValue)); order != 0 {
			return order
		}
	}
	if order := cmp.Compare(len(b.Match.Headers), len(a.Match.Headers)); order != 0 {
		return order
	}
	if order := a.Route.CreationTimestamp.Compare(b.Route.CreationTimestamp.Time); order != 0 {
		return order
	}
	if order := cmp.Compare(a.Route.Namespace+"/"+a.Route.Name, b.Route.Namespace+"/"+b.Route.Name); order != 0 {
		return order
	}
	if order := cmp.Compare(a.RuleIndex, b.RuleIndex); order != 0 {
		return order
	}
	if order := cmp.Compare(a.MatchIndex, b.MatchIndex); order != 0 {
		return order
	}
	return cmp.Compare(a.Hostname, b.Hostname)
}

func hostnameSpecificity(hostname string) (int, int) {
	if strings.HasPrefix(hostname, "*.") {
		return 0, len(hostname)
	}
	return len(hostname), len(hostname)
}
