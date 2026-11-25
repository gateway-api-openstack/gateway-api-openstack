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

package cloud

import (
	"fmt"
	"strconv"
	"strings"
)

// HTTPProtocol is the physical listener protocol implemented by the adapter.
const HTTPProtocol = "HTTP"

// ListenerKey identifies one physical listener within a Gateway.
type ListenerKey struct {
	Protocol string
	Port     int
}

// Validate checks the supported protocol and port range.
func (k ListenerKey) Validate() error {
	if k.Protocol != HTTPProtocol || k.Port < 1 || k.Port > 65535 {
		return fmt.Errorf("listener requires HTTP and a port within 1-65535")
	}
	return nil
}

// String returns the canonical listener child key.
func (k ListenerKey) String() string {
	return fmt.Sprintf("listener/%s/%d", k.Protocol, k.Port)
}

// ResourceKind distinguishes the child identities in a Gateway graph.
type ResourceKind uint8

const (
	// ResourceListener identifies a physical listener.
	ResourceListener ResourceKind = iota + 1
	// ResourceRule identifies a Route rule's pool, members, and monitor.
	ResourceRule
	// ResourcePolicy identifies one expanded match policy.
	ResourcePolicy
	// ResourcePredicate identifies one rule beneath an expanded policy.
	ResourcePredicate
)

// ResourceKey identifies a child within its immutable Gateway and Route tuple.
// Ordinals identify slots, not the mutable values assigned to those slots.
type ResourceKey struct {
	Kind       ResourceKind
	Listener   ListenerKey
	RuleIndex  int
	MatchIndex int
	HostIndex  int
	Variant    string
	Predicate  string
}

// String returns the canonical child key. ParseResourceKey validates it.
func (k ResourceKey) String() string {
	switch k.Kind {
	case ResourceListener:
		return k.Listener.String()
	case ResourceRule:
		return fmt.Sprintf("rule/%d", k.RuleIndex)
	case ResourcePolicy, ResourcePredicate:
		value := fmt.Sprintf("%s/rule/%d/match/%d/host/%d/path/%s", k.Listener, k.RuleIndex, k.MatchIndex, k.HostIndex, k.Variant)
		if k.Kind == ResourcePredicate {
			value += "/predicate/" + k.Predicate
		}
		return value
	default:
		return ""
	}
}

// ParseResourceKey validates the canonical child representation without using
// the current desired array lengths, so removed slots remain discoverable.
func ParseResourceKey(value string) (ResourceKey, error) {
	parts := strings.Split(value, "/")
	invalid := func() (ResourceKey, error) {
		return ResourceKey{}, fmt.Errorf("invalid resource key %q", value)
	}
	ordinal := func(value string) (int, bool) {
		n, err := strconv.Atoi(value)
		return n, err == nil && n >= 0 && strconv.Itoa(n) == value
	}
	if len(parts) == 2 && parts[0] == "rule" {
		index, ok := ordinal(parts[1])
		if !ok {
			return invalid()
		}
		return ResourceKey{Kind: ResourceRule, RuleIndex: index}, nil
	}
	if len(parts) != 3 && len(parts) != 11 && len(parts) != 13 || parts[0] != "listener" {
		return invalid()
	}
	port, ok := ordinal(parts[2])
	key := ResourceKey{Kind: ResourceListener, Listener: ListenerKey{Protocol: parts[1], Port: port}}
	if !ok || key.Listener.Validate() != nil {
		return invalid()
	}
	if len(parts) == 3 {
		return key, nil
	}
	if parts[3] != "rule" || parts[5] != "match" || parts[7] != "host" || parts[9] != "path" {
		return invalid()
	}
	for _, field := range []struct {
		index  int
		target *int
	}{{4, &key.RuleIndex}, {6, &key.MatchIndex}, {8, &key.HostIndex}} {
		value, valid := ordinal(parts[field.index])
		if !valid {
			return invalid()
		}
		*field.target = value
	}
	key.Kind = ResourcePolicy
	key.Variant = parts[10]
	if key.Variant != "exact" && key.Variant != "prefix" {
		return invalid()
	}
	if len(parts) == 13 {
		if parts[11] != "predicate" {
			return invalid()
		}
		key.Kind = ResourcePredicate
		key.Predicate = parts[12]
		if key.Predicate != "path" && key.Predicate != "host" {
			if !strings.HasPrefix(key.Predicate, "header-") {
				return invalid()
			}
			if _, valid := ordinal(strings.TrimPrefix(key.Predicate, "header-")); !valid {
				return invalid()
			}
		}
	}
	return key, nil
}
