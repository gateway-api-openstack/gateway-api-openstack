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
	"errors"
	"fmt"
	"slices"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Compile builds a deterministic HTTP model without reading or changing inputs.
// Input diagnostics remain in the returned model. They are not cloud deletion
// decisions, status patches, or permission to omit unsupported traffic rules.
func Compile(input Input) (Gateway, error) {
	if input.Gateway == nil {
		return Gateway{}, errors.New("gateway input is required")
	}
	result := Gateway{Source: source(input.Gateway), Listeners: compileListeners(input.Gateway)}
	if input.Gateway.Spec.AllowedListeners != nil || input.Gateway.Spec.TLS != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			Condition: string(gatewayv1.GatewayConditionAccepted), Reason: string(gatewayv1.GatewayReasonInvalidParameters),
			Message: "ListenerSets and Gateway-wide TLS are not implemented by the logical HTTP compiler",
		})
	}
	seenRoutes := make(map[string]bool, len(input.Routes))
	for i := range input.Routes {
		route := &input.Routes[i]
		key := route.Namespace + "/" + route.Name
		if seenRoutes[key] {
			return Gateway{}, fmt.Errorf("duplicate HTTPRoute input %s", key)
		}
		seenRoutes[key] = true
		var parents []int
		for index, parent := range route.Spec.ParentRefs {
			if targetsGateway(parent, route.Namespace, input.Gateway) {
				parents = append(parents, index)
			}
		}
		if len(parents) == 0 {
			continue
		}
		compiled := compileRoute(route, input)
		for _, index := range parents {
			parent := attachParent(input, route, index, result.Listeners)
			if len(compiled.Diagnostics) != 0 {
				parent.State = AttachmentRejected
				parent.Diagnostics = append(parent.Diagnostics, compiled.Diagnostics...)
			}
			compiled.Parents = append(compiled.Parents, parent)
		}
		result.Routes = append(result.Routes, compiled)
	}
	slices.SortFunc(result.Routes, func(a, b Route) int {
		return cmp.Compare(a.Source.Namespace+"/"+a.Source.Name, b.Source.Namespace+"/"+b.Source.Name)
	})
	for i := range result.Listeners {
		populateListener(&result.Listeners[i], result.Routes)
	}
	slices.SortFunc(result.Listeners, func(a, b Listener) int { return cmp.Compare(a.Name, b.Name) })
	return result, nil
}

func compileListeners(gateway *gatewayv1.Gateway) []Listener {
	result := make([]Listener, 0, len(gateway.Spec.Listeners))
	for _, listener := range gateway.Spec.Listeners {
		compiled := Listener{Name: listener.Name, Port: listener.Port, Protocol: listener.Protocol}
		if listener.Hostname != nil {
			compiled.Hostname = string(*listener.Hostname)
		}
		switch {
		case listener.Protocol != gatewayv1.HTTPProtocolType:
			compiled.Diagnostics = append(compiled.Diagnostics, Diagnostic{
				Condition: string(gatewayv1.ListenerConditionAccepted), Reason: string(gatewayv1.ListenerReasonUnsupportedProtocol),
				Message: "only HTTP listeners can be resolved without certificate inputs",
			})
		case listener.TLS != nil:
			compiled.Diagnostics = append(compiled.Diagnostics, Diagnostic{
				Condition: string(gatewayv1.ListenerConditionAccepted), Reason: string(gatewayv1.ListenerReasonUnsupportedValue),
				Message: "TLS configuration is unsupported on an HTTP listener",
			})
		case listener.Port < 1 || listener.Port > 65535:
			compiled.Diagnostics = append(compiled.Diagnostics, Diagnostic{
				Condition: string(gatewayv1.ListenerConditionAccepted), Reason: string(gatewayv1.ListenerReasonPortUnavailable),
				Message: "listener port must be between 1 and 65535",
			})
		}
		if listener.AllowedRoutes != nil {
			for _, kind := range listener.AllowedRoutes.Kinds {
				if kind.Kind != "HTTPRoute" || kind.Group != nil && *kind.Group != gatewayv1.GroupName {
					compiled.Diagnostics = append(compiled.Diagnostics, Diagnostic{
						Condition: string(gatewayv1.ListenerConditionResolvedRefs), Reason: string(gatewayv1.ListenerReasonInvalidRouteKinds),
						Message: "listener allowedRoutes includes a kind not supported by the HTTP compiler",
					})
					break
				}
			}
		}
		result = append(result, compiled)
	}
	// Only supported protocols participate in conflicts. In particular, an
	// unsupported TCP listener cannot displace HTTP on the same port.
	// Exact and wildcard hostnames may overlap, but identical HTTP hostnames
	// on the same port conflict regardless of input order or TLS configuration.
	for i := range result {
		if result[i].Protocol != gatewayv1.HTTPProtocolType {
			continue
		}
		for j := i + 1; j < len(result); j++ {
			if result[j].Protocol != gatewayv1.HTTPProtocolType || result[i].Port != result[j].Port {
				continue
			}
			if result[i].Hostname == result[j].Hostname {
				diagnostic := Diagnostic{Condition: string(gatewayv1.ListenerConditionConflicted), Reason: string(gatewayv1.ListenerReasonHostnameConflict), Message: "HTTP listeners on the same port have identical hostnames"}
				result[i].Diagnostics = append(result[i].Diagnostics, diagnostic)
				result[j].Diagnostics = append(result[j].Diagnostics, diagnostic)
			}
		}
	}
	for i := range result {
		slices.SortFunc(result[i].Diagnostics, func(a, b Diagnostic) int {
			return cmp.Compare(a.Condition+"/"+a.Reason+"/"+a.Message, b.Condition+"/"+b.Reason+"/"+b.Message)
		})
		result[i].Diagnostics = slices.Compact(result[i].Diagnostics)
	}
	return result
}

func compileRoute(route *gatewayv1.HTTPRoute, input Input) Route {
	result := Route{Source: source(route)}
	for _, hostname := range route.Spec.Hostnames {
		result.Hostnames = append(result.Hostnames, string(hostname))
	}
	slices.Sort(result.Hostnames)
	result.Hostnames = slices.Compact(result.Hostnames)
	if route.Spec.UseDefaultGateways != "" && route.Spec.UseDefaultGateways != "None" {
		result.Diagnostics = append(result.Diagnostics, *unsupported("default Gateway attachment is not implemented", ScopeRoute))
	}
	supportedRules := 0
	for i, rule := range route.Spec.Rules {
		compiled := Rule{Index: i, Backends: ResolveBackends(route, rule, input)}
		if rule.Name != nil || rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil {
			compiled.Diagnostics = append(compiled.Diagnostics, *unsupported("named rules, timeouts, retries, and session persistence are not implemented", ScopeRule))
		}
		for _, filter := range rule.Filters {
			scope := ScopeRule
			if !knownFilter(filter.Type) {
				scope = ScopeRoute
			}
			compiled.Diagnostics = append(compiled.Diagnostics, *unsupported(fmt.Sprintf("HTTPRoute filter %s requires an explicit action model before lowering", filter.Type), scope))
		}
		for _, backend := range rule.BackendRefs {
			if len(backend.Filters) != 0 {
				compiled.Diagnostics = append(compiled.Diagnostics, *unsupported("backendRef filters are not implemented", ScopeRule))
				break
			}
		}
		matches := rule.Matches
		if len(matches) == 0 {
			matches = []gatewayv1.HTTPRouteMatch{{}}
		}
		for matchIndex, match := range matches {
			normalized, err := NormalizeMatch(match)
			if err != nil {
				compiled.Diagnostics = append(compiled.Diagnostics, *err.(*Diagnostic))
				continue
			}
			normalized.Index = matchIndex
			compiled.Matches = append(compiled.Matches, normalized)
		}
		for _, diagnostic := range compiled.Diagnostics {
			if diagnostic.Scope == ScopeRoute {
				result.Diagnostics = append(result.Diagnostics, diagnostic)
			}
		}
		if len(compiled.Diagnostics) == 0 {
			supportedRules++
		}
		result.Rules = append(result.Rules, compiled)
	}
	if len(result.Rules) != 0 && supportedRules == 0 && len(result.Diagnostics) == 0 {
		result.Diagnostics = append(result.Diagnostics, *unsupported("HTTPRoute has no supported rules", ScopeRoute))
	}
	return result
}

func knownFilter(kind gatewayv1.HTTPRouteFilterType) bool {
	switch kind {
	case gatewayv1.HTTPRouteFilterRequestHeaderModifier, gatewayv1.HTTPRouteFilterResponseHeaderModifier,
		gatewayv1.HTTPRouteFilterRequestMirror, gatewayv1.HTTPRouteFilterRequestRedirect,
		gatewayv1.HTTPRouteFilterURLRewrite, gatewayv1.HTTPRouteFilterExtensionRef,
		gatewayv1.HTTPRouteFilterCORS, gatewayv1.HTTPRouteFilterExternalAuth:
		return true
	default:
		return false
	}
}

func populateListener(listener *Listener, routes []Route) {
	for _, route := range routes {
		hostnames := make([]string, 0)
		for _, parent := range route.Parents {
			if parent.State != AttachmentAccepted {
				continue
			}
			for _, attachment := range parent.Listeners {
				if attachment.Name == listener.Name && attachment.State == AttachmentAccepted {
					hostnames = append(hostnames, attachment.Hostnames...)
				}
			}
		}
		if len(hostnames) == 0 {
			continue
		}
		listener.AttachedRoutes++
		slices.Sort(hostnames)
		hostnames = slices.Compact(hostnames)
		for _, rule := range route.Rules {
			// Keep matches from unsupported action rules in the logical order.
			// Their diagnostics must block lowering rather than creating a hole
			// that could fall through to a broader route.
			for _, match := range rule.Matches {
				for _, hostname := range hostnames {
					listener.Rules = append(listener.Rules, RouteMatch{
						Route: route.Source, RuleIndex: rule.Index, MatchIndex: match.Index,
						Hostname: hostname, Match: match,
					})
				}
			}
		}
	}
	slices.SortStableFunc(listener.Rules, compareMatches)
}
