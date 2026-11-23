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
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
)

// Octavia 19.0.0 validates each L7 key and value at 255 characters and permits
// at most 50 predicates in one policy. Validate after translation.
const (
	octaviaL7ValueLimit    = 255
	octaviaPolicyRuleLimit = 50
)

type desiredPolicy struct {
	role        string
	key         cloud.ResourceKey
	match       *cloud.RouteMatchSpec
	position    int32
	compareType l7policies.CompareType
	path        string
}

type desiredRule struct {
	role        string
	key         cloud.ResourceKey
	header      string
	ruleType    l7policies.RuleType
	compareType l7policies.CompareType
	value       string
}

type desiredRoute struct {
	identity      Identity
	spec          cloud.RouteSpec
	rule          cloud.RouteRuleSpec
	members       []cloud.Member
	healthMonitor cloud.HealthMonitorConfig
	policies      []desiredPolicy
}

func (p *Provider) buildDesiredRule(spec cloud.RouteSpec, rule cloud.RouteRuleSpec) (desiredRoute, error) {
	identity, err := p.identity(spec.Identity)
	if err != nil {
		return desiredRoute{}, err
	}
	invalid := func(err error) (desiredRoute, error) {
		return desiredRoute{}, cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, err)
	}
	if rule.Index < 0 {
		return invalid(errors.New("route rule index must not be negative"))
	}
	for _, role := range []string{rolePool, roleMember, roleMonitor} {
		if _, err := identity.ChildTags(role, cloud.ResourceKey{Kind: cloud.ResourceRule, RuleIndex: rule.Index}); err != nil {
			return invalid(err)
		}
	}
	switch spec.MemberMode {
	case "", cloud.MemberModeNodePort:
		spec.MemberMode = cloud.MemberModeNodePort
	case cloud.MemberModeEndpoint:
		if len(spec.BackendSecurityGroupIDs) != 0 {
			return invalid(errors.New("endpoint members require unmanaged backend security"))
		}
	default:
		return invalid(fmt.Errorf("unsupported member mode %q", spec.MemberMode))
	}
	if len(rule.Members) == 0 && spec.MemberMode != cloud.MemberModeEndpoint {
		return invalid(errors.New("at least one ready backend member is required"))
	}
	if strings.TrimSpace(spec.MemberSubnetID) == "" {
		return invalid(errors.New("member subnet ID must not be empty"))
	}

	membersByKey := make(map[string]cloud.Member, len(rule.Members))
	for _, member := range rule.Members {
		if member.Port < 1 || member.Port > 65535 {
			return invalid(fmt.Errorf("member %s has invalid port %d", member.Address, member.Port))
		}
		address, err := netip.ParseAddr(member.Address)
		if err != nil {
			return invalid(fmt.Errorf("member address %q is not a valid IP address: %w", member.Address, err))
		}
		if spec.MemberMode == cloud.MemberModeEndpoint {
			if !address.Is4() || !address.IsGlobalUnicast() || address.IsLoopback() || address.IsLinkLocalUnicast() {
				return invalid(errors.New("endpoint members require unicast IPv4 destinations"))
			}
			if member.ProviderID != "" {
				return invalid(errors.New("endpoint members must not identify a worker provider instance"))
			}
		} else if member.Draining {
			return invalid(errors.New("draining members require endpoint mode"))
		}
		member.Address = address.String()
		key := memberKey(member.Address, member.Port)
		if existing, duplicate := membersByKey[key]; duplicate && (existing.ProviderID != member.ProviderID || existing.Draining != member.Draining) {
			return invalid(errors.New("members with the same address and port must have consistent state"))
		}
		membersByKey[key] = member
	}
	keys := make([]string, 0, len(membersByKey))
	for key := range membersByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	members := make([]cloud.Member, 0, len(keys))
	for _, key := range keys {
		members = append(members, membersByKey[key])
	}

	monitor := spec.HealthMonitor
	if !strings.HasPrefix(monitor.Path, "/") {
		return invalid(fmt.Errorf("health check path %q must start with /", monitor.Path))
	}
	if monitor.Delay < 1 || monitor.Timeout < 1 || monitor.Timeout >= monitor.Delay ||
		monitor.MaxRetries < 1 || monitor.MaxRetries > 10 || monitor.MaxRetriesDown < 1 || monitor.MaxRetriesDown > 10 {
		return invalid(errors.New("health monitor requires positive intervals, timeout below delay, and retry counts within 1-10"))
	}
	policies, err := policiesFor(rule)
	if err != nil {
		return invalid(err)
	}
	return desiredRoute{
		identity:      identity,
		spec:          spec,
		rule:          rule,
		members:       members,
		healthMonitor: monitor,
		policies:      policies,
	}, nil
}

// policiesFor expands each alternative only after the complete listener order
// has been calculated by the controller. Key ordinals are independent of values.
func policiesFor(rule cloud.RouteRuleSpec) ([]desiredPolicy, error) {
	if len(rule.Matches) == 0 {
		return nil, errors.New("forwarding rule requires at least one match")
	}
	var policies []desiredPolicy
	keys := make(map[cloud.ResourceKey]struct{})
	for _, match := range rule.Matches {
		if err := match.Listener.Validate(); err != nil {
			return nil, err
		}
		if match.MatchIndex < 0 || match.HostIndex < 0 || match.Priority < 0 {
			return nil, errors.New("match ordinals and priority must not be negative")
		}
		if match.PathType != cloud.PathMatchExact && match.PathType != cloud.PathMatchPrefix {
			return nil, fmt.Errorf("unsupported path match type %q", match.PathType)
		}
		path := match.PathValue
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(path, "/") {
			return nil, errors.New("match path must start with /")
		}
		key := cloud.ResourceKey{Kind: cloud.ResourcePolicy, Listener: match.Listener, RuleIndex: rule.Index,
			MatchIndex: match.MatchIndex, HostIndex: match.HostIndex, Variant: "exact"}
		expanded := []desiredPolicy{{role: rolePolicyExact, key: key, match: &match, compareType: l7policies.CompareTypeEqual, path: path}}
		if match.PathType == cloud.PathMatchPrefix {
			path = strings.TrimRight(path, "/")
			if path == "" {
				key.Variant = "prefix"
				expanded = []desiredPolicy{{role: rolePolicyPrefix, key: key, match: &match, compareType: l7policies.CompareTypeStartWith, path: "/"}}
			} else {
				expanded[0].path = path
				key.Variant = "prefix"
				expanded = append(expanded, desiredPolicy{role: rolePolicyPrefix, key: key, match: &match, compareType: l7policies.CompareTypeStartWith, path: path + "/"})
			}
		}
		for _, policy := range expanded {
			if _, exists := keys[policy.key]; exists {
				return nil, errors.New("duplicate policy child key")
			}
			keys[policy.key] = struct{}{}
			rules := rulesFor(policy)
			if len(rules) > octaviaPolicyRuleLimit {
				return nil, errors.New("match exceeds the Octavia policy rule limit")
			}
			for _, predicate := range rules {
				if len(predicate.value) == 0 || utf8.RuneCountInString(predicate.value) > octaviaL7ValueLimit || utf8.RuneCountInString(predicate.header) > octaviaL7ValueLimit || strings.ContainsAny(predicate.value, "\r\n\x00") {
					return nil, errors.New("match exceeds the Octavia L7 key or value representation")
				}
			}
			policies = append(policies, policy)
		}
	}
	return policies, nil
}

func rulesFor(policy desiredPolicy) []desiredRule {
	predicate := func(name string) cloud.ResourceKey {
		key := policy.key
		key.Kind = cloud.ResourcePredicate
		key.Predicate = name
		return key
	}
	rules := []desiredRule{{role: roleRulePath, key: predicate("path"), ruleType: l7policies.TypePath,
		compareType: policy.compareType, value: policy.path}}
	if hostname := policy.match.Hostname; hostname != "" {
		// Host is case-insensitive in Amphora. An optional port is not part of
		// Gateway API hostname matching, including for exact hostnames.
		value := regexp.QuoteMeta(hostname)
		if strings.HasPrefix(hostname, "*.") {
			value = "([^.]+\\.)+" + regexp.QuoteMeta(strings.TrimPrefix(hostname, "*."))
		}
		rules = append(rules, desiredRule{role: roleRuleHost, key: predicate("host"), ruleType: l7policies.TypeHostName,
			compareType: l7policies.CompareTypeRegex, value: "^" + value + "(:[0-9]+)?$"})
	}
	for index, header := range policy.match.Headers {
		rules = append(rules, desiredRule{role: roleRuleHeader, key: predicate(fmt.Sprintf("header-%d", index)),
			ruleType: l7policies.TypeHeader, compareType: l7policies.CompareTypeEqual,
			header: header.Name, value: header.Value})
	}
	return rules
}

func memberKey(address string, port int) string {
	parsed, err := netip.ParseAddr(address)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Sprintf("%s:%d", address, port)
	}
	return netip.AddrPortFrom(parsed, uint16(port)).String()
}
