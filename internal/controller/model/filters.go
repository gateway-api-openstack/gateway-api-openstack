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
	"net/http"
	"slices"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Filter preserves rule-level filter order independently of backend selection.
// Its presence does not establish an Octavia representation or capability.
type Filter struct {
	Type           gatewayv1.HTTPRouteFilterType
	RequestHeaders *RequestHeaders
	Redirect       *Redirect
}

// RequestHeaders contains case-normalized request header operations.
type RequestHeaders struct {
	Set    []Header
	Add    []Header
	Remove []string
}

// Header contains a case-insensitive name and an unchanged header value.
type Header struct {
	Name  string
	Value string
}

// Redirect preserves omitted replacements until the request and listener are
// known. In particular, a missing hostname preserves the request hostname and a
// missing scheme and port use the request scheme and Gateway listener port.
type Redirect struct {
	Scheme     *string
	Hostname   *gatewayv1.PreciseHostname
	Path       *gatewayv1.HTTPPathModifier
	Port       *gatewayv1.PortNumber
	StatusCode int
}

func compileFilters(rule gatewayv1.HTTPRouteRule) ([]Filter, []Diagnostic) {
	var result []Filter
	var diagnostics []Diagnostic
	seen := make(map[gatewayv1.HTTPRouteFilterType]bool, len(rule.Filters))
	for _, filter := range rule.Filters {
		compiled := Filter{Type: filter.Type}
		var diagnostic *Diagnostic
		switch filter.Type {
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			compiled.RequestHeaders, diagnostic = compileRequestHeaders(filter.RequestHeaderModifier)
		case gatewayv1.HTTPRouteFilterRequestRedirect:
			compiled.Redirect, diagnostic = compileRedirect(filter.RequestRedirect)
		default:
			scope := ScopeRule
			if !knownFilter(filter.Type) {
				scope = ScopeRoute
			}
			diagnostic = unsupported(fmt.Sprintf("HTTPRoute filter %s is not implemented by the logical HTTP compiler", filter.Type), scope)
		}
		if diagnostic != nil {
			diagnostics = append(diagnostics, *diagnostic)
		}
		if seen[filter.Type] && (filter.Type == gatewayv1.HTTPRouteFilterRequestHeaderModifier || filter.Type == gatewayv1.HTTPRouteFilterRequestRedirect) {
			diagnostics = append(diagnostics, *incompatibleFilters("a Core HTTPRoute filter cannot be repeated"))
		}
		seen[filter.Type] = true
		result = append(result, compiled)
	}
	if seen[gatewayv1.HTTPRouteFilterRequestRedirect] {
		if seen[gatewayv1.HTTPRouteFilterURLRewrite] {
			diagnostics = append(diagnostics, *incompatibleFilters("RequestRedirect and URLRewrite cannot be combined"))
		}
		if len(rule.BackendRefs) != 0 {
			diagnostics = append(diagnostics, *incompatibleFilters("RequestRedirect cannot be combined with backendRefs"))
		}
	}
	return result, diagnostics
}

func compileRequestHeaders(input *gatewayv1.HTTPHeaderFilter) (*RequestHeaders, *Diagnostic) {
	if input == nil {
		return nil, unsupported("RequestHeaderModifier requires requestHeaderModifier configuration", ScopeRule)
	}
	result := &RequestHeaders{Set: normalizeHeaders(input.Set), Add: normalizeHeaders(input.Add)}
	for _, name := range input.Remove {
		result.Remove = append(result.Remove, strings.ToLower(name))
	}
	slices.Sort(result.Remove)
	result.Remove = slices.Compact(result.Remove)

	// A header can have only one operation, even when its spelling differs
	// between the add, set, and remove lists.
	seen := make(map[string]bool, len(result.Set)+len(result.Add)+len(result.Remove))
	for _, headers := range [][]Header{result.Set, result.Add} {
		for _, header := range headers {
			if seen[header.Name] {
				return result, incompatibleFilters("request header names must have only one operation")
			}
			seen[header.Name] = true
		}
	}
	for _, name := range result.Remove {
		if seen[name] {
			return result, incompatibleFilters("request header names must have only one operation")
		}
	}
	return result, nil
}

func normalizeHeaders(input []gatewayv1.HTTPHeader) []Header {
	var result []Header
	seen := make(map[string]bool, len(input))
	for _, header := range input {
		name := strings.ToLower(string(header.Name))
		// HTTPHeader specifies that the first case-equivalent entry wins.
		if seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, Header{Name: name, Value: header.Value})
	}
	slices.SortFunc(result, func(a, b Header) int { return cmp.Compare(a.Name, b.Name) })
	return result
}

func compileRedirect(input *gatewayv1.HTTPRequestRedirectFilter) (*Redirect, *Diagnostic) {
	if input == nil {
		return nil, unsupported("RequestRedirect requires requestRedirect configuration", ScopeRule)
	}
	copy := input.DeepCopy()
	result := &Redirect{
		Scheme: copy.Scheme, Hostname: copy.Hostname, Path: copy.Path, Port: copy.Port,
		StatusCode: http.StatusFound,
	}
	if copy.StatusCode != nil {
		result.StatusCode = *copy.StatusCode
	}
	if input.Scheme != nil && *input.Scheme != "http" && *input.Scheme != "https" {
		return result, unsupported("unknown redirect scheme", ScopeRoute)
	}
	if input.Path != nil && input.Path.Type != gatewayv1.FullPathHTTPPathModifier && input.Path.Type != gatewayv1.PrefixMatchHTTPPathModifier {
		return result, unsupported("unknown redirect path modifier", ScopeRoute)
	}
	switch result.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound:
	case http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		// The pinned Gateway API includes these values in the standard schema
		// but lists them separately in pkg/features.HTTPRouteExtendedFeatures.
		return result, unsupported("redirect status code is outside the Core HTTP profile", ScopeRule)
	default:
		return result, unsupported("unknown redirect status code", ScopeRoute)
	}
	if input.Scheme != nil || input.Port != nil || input.Path != nil {
		return result, unsupported("redirect scheme, port, and path replacement are outside the Core HTTP profile", ScopeRule)
	}
	return result, nil
}

func incompatibleFilters(message string) *Diagnostic {
	return &Diagnostic{Condition: string(gatewayv1.RouteConditionAccepted), Reason: string(gatewayv1.RouteReasonIncompatibleFilters), Message: message, Scope: ScopeRule}
}
