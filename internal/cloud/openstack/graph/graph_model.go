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

package graph

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func (p *Provider) buildDesiredGatewayGraph(spec cloud.GatewayGraphSpec) (desiredGatewayGraph, error) {
	identity, err := p.identity(spec.Identity)
	if err != nil {
		return desiredGatewayGraph{}, err
	}
	listenerKeys, err := validateGatewayGraphNode(spec, identity)
	if err != nil {
		return desiredGatewayGraph{}, err
	}
	spec.Gateway.Spec.Listeners = slices.Clone(spec.Gateway.Spec.Listeners)
	sort.Slice(spec.Gateway.Spec.Listeners, func(i, j int) bool {
		return spec.Gateway.Spec.Listeners[i].Key.String() < spec.Gateway.Spec.Listeners[j].Key.String()
	})
	spec.Routes = slices.Clone(spec.Routes)
	sort.Slice(spec.Routes, func(i, j int) bool {
		a, b := spec.Routes[i].Spec.Identity, spec.Routes[j].Spec.Identity
		if a.RouteNamespace != b.RouteNamespace {
			return a.RouteNamespace < b.RouteNamespace
		}
		if a.RouteName != b.RouteName {
			return a.RouteName < b.RouteName
		}
		return a.RouteUID < b.RouteUID
	})
	desired := desiredGatewayGraph{spec: spec, identity: identity, routes: make([][]desiredRoute, len(spec.Routes))}
	identities := make(map[string]struct{}, len(spec.Routes))
	matches := gatewayMatchIndex{listeners: listenerKeys, priorities: make(map[cloud.ListenerKey]map[int]struct{}), policies: make(map[cloud.ListenerKey][]*desiredPolicy)}
	for index, node := range spec.Routes {
		if err := validateRouteGraphNode(spec.Identity, node, identities); err != nil {
			return desiredGatewayGraph{}, err
		}
		routeIdentity, err := p.identity(node.Spec.Identity)
		if err != nil {
			return desiredGatewayGraph{}, err
		}
		desired.identities = append(desired.identities, routeIdentity)
		if node.Disposition != cloud.GraphEnsure {
			continue
		}
		desired.routes[index], err = p.buildGatewayRouteRules(spec.Gateway, node.Spec, routeIdentity, &matches)
		if err != nil {
			return desiredGatewayGraph{}, err
		}
	}
	matches.assignPolicyPositions()
	return desired, nil
}

func graphValidationError(message string) error {
	return cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, errors.New(message))
}

func validateGatewayGraphNode(spec cloud.GatewayGraphSpec, identity Identity) (map[cloud.ListenerKey]struct{}, error) {
	if spec.Identity.RouteUID != "" || spec.Identity.RouteName != "" || spec.Identity.RouteNamespace != "" {
		return nil, graphValidationError("graph identity must not contain HTTPRoute fields")
	}
	if !validGraphDisposition(spec.Gateway.Disposition) {
		return nil, graphValidationError("Gateway graph disposition is invalid")
	}
	keys := make(map[cloud.ListenerKey]struct{})
	if spec.Gateway.Disposition != cloud.GraphEnsure {
		return keys, nil
	}
	if !sameGatewayIdentity(spec.Identity, spec.Gateway.Spec.Identity) {
		return nil, fmt.Errorf("%w: Gateway model and graph identities differ", cloud.ErrOwnershipConflict)
	}
	if spec.Gateway.Spec.Provider != "amphora" || strings.TrimSpace(spec.Gateway.Spec.VIPSubnetID) == "" || len(spec.Gateway.Spec.Listeners) == 0 {
		return nil, graphValidationError("Gateway requires Amphora, a VIP subnet, and HTTP listeners")
	}
	for _, listener := range spec.Gateway.Spec.Listeners {
		if err := listener.Key.Validate(); err != nil {
			return nil, graphValidationError(err.Error())
		}
		if _, duplicate := keys[listener.Key]; duplicate {
			return nil, graphValidationError("duplicate physical listener key")
		}
		keys[listener.Key] = struct{}{}
		if _, err := identity.ChildTags(roleListener, cloud.ResourceKey{Kind: cloud.ResourceListener, Listener: listener.Key}); err != nil {
			return nil, graphValidationError(err.Error())
		}
	}
	if _, err := identity.GatewayTags(roleLoadBalancer); err != nil {
		return nil, graphValidationError(err.Error())
	}
	return keys, nil
}

func validateRouteGraphNode(gateway cloud.Identity, node cloud.RouteGraphNode, seen map[string]struct{}) error {
	id := node.Spec.Identity
	if !validGraphDisposition(node.Disposition) {
		return graphValidationError("HTTPRoute graph disposition is invalid")
	}
	if err := cloud.ValidateRouteIdentity(id); err != nil {
		return &cloud.GraphError{Identity: id, Err: err}
	}
	if !sameGatewayIdentity(gateway, id) {
		return &cloud.GraphError{Identity: id, Err: fmt.Errorf("%w: HTTPRoute belongs to a different Gateway graph", cloud.ErrOwnershipConflict)}
	}
	if _, duplicate := seen[id.RouteUID]; duplicate {
		return fmt.Errorf("%w: duplicate HTTPRoute fragment", cloud.ErrOwnershipConflict)
	}
	seen[id.RouteUID] = struct{}{}
	return nil
}

func (p *Provider) buildGatewayRouteRules(gateway cloud.GatewayGraphNode, spec cloud.RouteSpec, identity Identity, matches *gatewayMatchIndex) ([]desiredRoute, error) {
	if gateway.Disposition != cloud.GraphEnsure {
		return nil, graphValidationError("an ensured HTTPRoute requires an ensured Gateway")
	}
	if !spec.GatewayRequirements.Equal(gatewayRequirements(gateway.Spec)) {
		return nil, &cloud.GraphError{Identity: spec.Identity, Err: fmt.Errorf("%w: HTTPRoute and Gateway infrastructure requirements differ", cloud.ErrOwnershipConflict)}
	}
	if len(spec.Rules) == 0 {
		return nil, graphValidationError("ensured HTTPRoute requires forwarding rules")
	}
	var routes []desiredRoute
	indexes := make(map[int]struct{}, len(spec.Rules))
	for _, rule := range spec.Rules {
		if _, duplicate := indexes[rule.Index]; duplicate {
			return nil, graphValidationError("duplicate HTTPRoute rule index")
		}
		indexes[rule.Index] = struct{}{}
		if err := matches.validateRule(rule); err != nil {
			return nil, err
		}
		compiled, err := p.buildDesiredRule(spec, rule)
		if err != nil {
			return nil, &cloud.GraphError{Identity: spec.Identity, Err: err}
		}
		if err := matches.addPolicies(identity, compiled.policies); err != nil {
			return nil, err
		}
		routes = append(routes, compiled)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].rule.Index < routes[j].rule.Index })
	return routes, nil
}

// gatewayMatchIndex keeps precedence and expanded Octavia positions local to
// each physical listener, including matches from different Routes and rules.
type gatewayMatchIndex struct {
	listeners  map[cloud.ListenerKey]struct{}
	priorities map[cloud.ListenerKey]map[int]struct{}
	policies   map[cloud.ListenerKey][]*desiredPolicy
}

func (index *gatewayMatchIndex) validateRule(rule cloud.RouteRuleSpec) error {
	for _, match := range rule.Matches {
		if _, present := index.listeners[match.Listener]; !present {
			return graphValidationError("route match references an unknown physical listener")
		}
		if index.priorities[match.Listener] == nil {
			index.priorities[match.Listener] = make(map[int]struct{})
		}
		if _, duplicate := index.priorities[match.Listener][match.Priority]; duplicate {
			return graphValidationError("listener matches must have distinct priorities")
		}
		index.priorities[match.Listener][match.Priority] = struct{}{}
	}
	return nil
}

func (index *gatewayMatchIndex) addPolicies(identity Identity, policies []desiredPolicy) error {
	for i := range policies {
		policy := &policies[i]
		if _, err := identity.ChildTags(policy.role, policy.key); err != nil {
			return graphValidationError(err.Error())
		}
		for _, predicate := range rulesFor(*policy) {
			if _, err := identity.ChildTags(predicate.role, predicate.key); err != nil {
				return graphValidationError(err.Error())
			}
		}
		index.policies[policy.key.Listener] = append(index.policies[policy.key.Listener], policy)
	}
	return nil
}

func (index *gatewayMatchIndex) assignPolicyPositions() {
	for _, policies := range index.policies {
		sort.SliceStable(policies, func(i, j int) bool { return policies[i].match.Priority < policies[j].match.Priority })
		for i, policy := range policies {
			policy.position = int32(i + 1)
		}
	}
}

func validGraphDisposition(disposition cloud.GraphDisposition) bool {
	return disposition == cloud.GraphEnsure || disposition == cloud.GraphPreserve || disposition == cloud.GraphDelete
}

func sameGatewayIdentity(a, b cloud.Identity) bool {
	return a.OpenStackProjectID == b.OpenStackProjectID && a.ClusterID == b.ClusterID && a.Controller == b.Controller &&
		a.GatewayNamespace == b.GatewayNamespace && a.GatewayName == b.GatewayName && a.GatewayUID == b.GatewayUID
}

func gatewayRequirements(spec cloud.GatewaySpec) cloud.GatewayRequirements {
	return cloud.GatewayRequirements{Provider: spec.Provider, VIPSubnetID: spec.VIPSubnetID, ExternalNetworkID: spec.ExternalNetworkID,
		FlavorID: spec.FlavorID, AvailabilityZone: spec.AvailabilityZone, VIPSecurityGroupIDs: spec.VIPSecurityGroupIDs}
}
