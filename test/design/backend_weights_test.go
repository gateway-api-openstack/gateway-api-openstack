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

package design

import (
	"math/big"
	"testing"
)

// These arithmetic proofs examine a single pool with uniform weights within
// each Service. They are
// not a controller compiler, HAProxy simulation, or traffic distribution test.
// Octavia 19.0.0 permits integer member weights in [0, 256]. Searching that
// entire domain provides counterexamples to unrestricted exact normalization.
// It does not disprove other mappings or approximate conformance distributions.
func TestBoundedMemberWeights(t *testing.T) {
	for _, test := range []struct {
		name      string
		services  [2]int
		endpoints [2]int
		want      [2]int
		exact     bool
	}{
		{name: "equal Services with unequal endpoints", services: [2]int{1, 1}, endpoints: [2]int{2, 1}, want: [2]int{1, 2}, exact: true},
		{name: "one to three Services", services: [2]int{1, 3}, endpoints: [2]int{2, 3}, want: [2]int{1, 2}, exact: true},
		{name: "upstream seven to three ratio", services: [2]int{7, 3}, endpoints: [2]int{2, 3}, want: [2]int{7, 2}, exact: true},
		{name: "endpoint added", services: [2]int{1, 3}, endpoints: [2]int{3, 3}, want: [2]int{1, 3}, exact: true},
		{name: "zero weight", services: [2]int{1, 0}, endpoints: [2]int{2, 3}, want: [2]int{1, 0}, exact: true},
		{name: "endpoint ratio exceeds bound", services: [2]int{1, 1}, endpoints: [2]int{257, 1}},
		{name: "Service ratio exceeds bound", services: [2]int{1, 257}, endpoints: [2]int{1, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var witness [2]int
			found := false
			for first := 0; first <= 256 && !found; first++ {
				for second := 0; second <= 256; second++ {
					if first+second == 0 {
						continue
					}
					// Cross multiplication avoids float rounding near the bound.
					if test.endpoints[0]*first*test.services[1] == test.endpoints[1]*second*test.services[0] {
						witness, found = [2]int{first, second}, true
						break
					}
				}
			}
			if found != test.exact || found && witness != test.want {
				t.Fatalf("member weight witness=%v exact=%t, want %v exact=%t", witness, found, test.want, test.exact)
			}
		})
	}
}

func TestEndpointHealthChangesServiceSharesWithoutReconciliation(t *testing.T) {
	// Services A:B have weight 1:3 and 2:3 endpoints. Per-endpoint weights 1:2
	// produce 2:6 while every endpoint is healthy. Octavia can remove a failed
	// endpoint from balancing before Kubernetes input or reconciliation changes.
	for _, test := range []struct {
		name      string
		healthy   [2]int
		wantShare string
	}{
		{name: "all healthy", healthy: [2]int{2, 3}, wantShare: "1/4"},
		{name: "one A endpoint fails", healthy: [2]int{1, 3}, wantShare: "1/7"},
		{name: "one B endpoint fails", healthy: [2]int{2, 2}, wantShare: "1/3"},
		{name: "all B endpoints fail", healthy: [2]int{2, 0}, wantShare: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			share := big.NewRat(int64(test.healthy[0]), int64(test.healthy[0]+2*test.healthy[1]))
			if share.RatString() != test.wantShare {
				t.Fatalf("A share=%s, want %s", share.RatString(), test.wantShare)
			}
		})
	}
}
