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

	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

// HTTPForwarding contains the physical HTTP mapping before member selection.
// It does not establish bindings or authorize mutating the observed graph.
type HTTPForwarding struct {
	Listeners []cloud.ListenerSpec
	Routes    []ForwardingRoute
}

// ForwardingRoute keeps every lowered rule within its exact source identity.
type ForwardingRoute struct {
	Source Source
	Rules  []ForwardingRule
}

// ForwardingRule connects the physical match alternatives to one Service group.
// Member selection is performed separately using the Gateway's frozen mode.
type ForwardingRule struct {
	Spec    cloud.RouteRuleSpec
	Backend BackendGroup
}

type forwardingRuleKey struct {
	route types.NamespacedName
	uid   types.UID
	rule  int
}

type forwardingRulePosition struct {
	route int
	rule  int
}

type forwardingMatchKey struct {
	forwardingRuleKey
	match int
}

// LowerHTTPForwarding maps ordinary forwarding without reading Kubernetes or
// cloud resources. It rejects unsupported actions and invalid backend groups
// before returning any plan, rather than creating a hole in the routing order.
// Runtime admission remains responsible for the lifecycle behaviors that need
// response APIs not yet exposed by Octavia.
func LowerHTTPForwarding(logical Gateway) (HTTPForwarding, error) {
	if len(logical.Diagnostics) != 0 {
		return HTTPForwarding{}, &logical.Diagnostics[0]
	}
	physical := make(map[cloud.ListenerKey][]RouteMatch)
	eligible := make(map[gatewayv1.SectionName]bool)
	for _, listener := range logical.Listeners {
		if len(listener.Diagnostics) != 0 || listener.Protocol != gatewayv1.HTTPProtocolType || listener.TLS != nil {
			continue
		}
		eligible[listener.Name] = true
		key := cloud.ListenerKey{Protocol: cloud.HTTPProtocol, Port: int(listener.Port)}
		physical[key] = append(physical[key], listener.Rules...)
	}

	if len(physical) == 0 {
		return HTTPForwarding{}, unsupported("Gateway has no valid HTTP listeners", ScopeRoute)
	}
	result := HTTPForwarding{}
	positions := make(map[forwardingRuleKey]forwardingRulePosition)
	for _, route := range logical.Routes {
		if !hasEligibleListener(route, eligible) {
			continue
		}
		if len(route.Diagnostics) != 0 {
			return HTTPForwarding{}, fmt.Errorf("lower HTTPRoute %s/%s: %w", route.Source.Namespace, route.Source.Name, &route.Diagnostics[0])
		}
		forwarding := ForwardingRoute{Source: route.Source}
		for _, rule := range route.Rules {
			backend, err := forwardingBackend(rule)
			if err != nil {
				return HTTPForwarding{}, fmt.Errorf("lower HTTPRoute %s/%s rule %d: %w", route.Source.Namespace, route.Source.Name, rule.Index, err)
			}
			positions[ruleKey(route.Source, rule.Index)] = forwardingRulePosition{route: len(result.Routes), rule: len(forwarding.Rules)}
			forwarding.Rules = append(forwarding.Rules, ForwardingRule{
				Spec: cloud.RouteRuleSpec{Index: rule.Index}, Backend: backend,
			})
		}
		result.Routes = append(result.Routes, forwarding)
	}

	keys := make([]cloud.ListenerKey, 0, len(physical))
	for key := range physical {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b cloud.ListenerKey) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
	for _, key := range keys {
		result.Listeners = append(result.Listeners, cloud.ListenerSpec{Key: key})
		matches := physical[key]
		slices.SortFunc(matches, compareMatches)
		matches = slices.CompactFunc(matches, sameForwardingMatch)
		hostnames := forwardingHostnames(matches)
		for priority, match := range matches {
			rule := ruleKey(match.Route, match.RuleIndex)
			position, found := positions[rule]
			if !found {
				return HTTPForwarding{}, fmt.Errorf("listener %s refers to an unlowered HTTPRoute rule", key)
			}
			hostIndex, _ := slices.BinarySearch(hostnames[forwardingMatchKey{forwardingRuleKey: rule, match: match.MatchIndex}], match.Hostname)
			lowered := cloud.RouteMatchSpec{
				Listener: key, MatchIndex: match.MatchIndex, HostIndex: hostIndex, Priority: priority,
				Hostname: match.Hostname, PathType: cloud.PathMatchType(match.Match.PathType), PathValue: match.Match.PathValue,
			}
			for _, header := range match.Match.Headers {
				lowered.Headers = append(lowered.Headers, cloud.HeaderMatch{Name: header.Name, Value: header.Value})
			}
			forwarding := &result.Routes[position.route].Rules[position.rule]
			forwarding.Spec.Matches = append(forwarding.Spec.Matches, lowered)
		}
	}
	return result, nil
}

func ruleKey(source Source, rule int) forwardingRuleKey {
	return forwardingRuleKey{route: types.NamespacedName{Namespace: source.Namespace, Name: source.Name}, uid: source.UID, rule: rule}
}

func hasEligibleListener(route Route, eligible map[gatewayv1.SectionName]bool) bool {
	for _, parent := range route.Parents {
		for _, listener := range parent.Listeners {
			if listener.State == AttachmentAccepted && eligible[listener.Name] {
				return true
			}
		}
	}
	return false
}

func forwardingBackend(rule Rule) (BackendGroup, error) {
	if len(rule.Diagnostics) != 0 {
		return BackendGroup{}, &rule.Diagnostics[0]
	}
	if len(rule.Filters) != 0 {
		return BackendGroup{}, unsupported("HTTP filters require an implemented Octavia action mapping", ScopeRule)
	}
	if len(rule.Backends) != 1 || rule.Backends[0].Weight <= 0 {
		return BackendGroup{}, unsupported("forwarding requires one backend with positive weight", ScopeRule)
	}
	backend := rule.Backends[0]
	if backend.Diagnostic != nil {
		return BackendGroup{}, backend.Diagnostic
	}
	if backend.Authorization != BackendAuthorizationAllowed || backend.State != BackendResolved || backend.Port == nil {
		return BackendGroup{}, unsupported("forwarding requires an authorized and resolved Service port", ScopeRule)
	}
	return backend, nil
}

func sameForwardingMatch(a, b RouteMatch) bool {
	return ruleKey(a.Route, a.RuleIndex) == ruleKey(b.Route, b.RuleIndex) && a.MatchIndex == b.MatchIndex && a.Hostname == b.Hostname
}

func forwardingHostnames(matches []RouteMatch) map[forwardingMatchKey][]string {
	hostnames := make(map[forwardingMatchKey][]string)
	for _, match := range matches {
		key := forwardingMatchKey{forwardingRuleKey: ruleKey(match.Route, match.RuleIndex), match: match.MatchIndex}
		hostnames[key] = append(hostnames[key], match.Hostname)
	}
	for key, names := range hostnames {
		slices.Sort(names)
		hostnames[key] = slices.Compact(names)
	}
	return hostnames
}
