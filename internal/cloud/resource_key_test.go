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

import "testing"

func TestResourceKeyCanonicalParsing(t *testing.T) {
	for _, value := range []string{
		"listener/HTTP/80", "listener/HTTP/65535", "rule/0", "rule/63",
		"listener/HTTP/8080/rule/3/match/7/host/2/path/prefix",
		"listener/HTTP/80/rule/0/match/0/host/0/path/exact/predicate/path",
		"listener/HTTP/80/rule/0/match/0/host/0/path/exact/predicate/host",
		"listener/HTTP/80/rule/0/match/0/host/0/path/exact/predicate/header-15",
	} {
		t.Run(value, func(t *testing.T) {
			key, err := ParseResourceKey(value)
			if err != nil || key.String() != value {
				t.Fatalf("round trip = %q, %v", key.String(), err)
			}
		})
	}
	for _, value := range []string{
		"", "listener/HTTP/0", "listener/HTTP/65536", "listener/TCP/80", "listener/http/80",
		"listener/HTTP/080", "rule/-1", "rule/+1", "rule/01", "rule/9223372036854775808", "rule/0/trailing",
		"listener/HTTP/80/rule/0/match/0/host/0/path/regex",
		"listener/HTTP/80/rule/0/match/0/host/0/path/exact/predicate/header--1",
		"listener/HTTP/80/rule/0/match/0/host/0/path/exact/predicate/header-01",
		"listener/HTTP/80/rule/0/match/0/host/0/path/exact/predicate/unknown",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseResourceKey(value); err == nil {
				t.Fatal("accepted invalid child key")
			}
		})
	}
}
