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

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
)

func TestNetworkEvidenceDistinguishesFailureKindsWithoutRawErrors(t *testing.T) {
	private := errors.New("secret-token private-project https://private-api.example.test")
	for _, test := range []struct {
		cause error
		kind  string
	}{
		{cause: nil},
		{cause: fmt.Errorf("%s: %w", private, context.DeadlineExceeded), kind: "context-deadline"},
		{cause: context.Canceled, kind: "context-canceled"},
		{cause: &net.DNSError{Err: private.Error(), IsTimeout: true}, kind: "transport-timeout"},
		{cause: cloud.NewProviderError(cloud.ErrorCategoryTimeout, private), kind: "provider-timeout"},
		{cause: private, kind: "other"},
	} {
		evidence := &networkEvidence{}
		before := time.Now()
		index := evidence.begin(networkObserveReplacement)
		evidence.complete(index, test.cause)
		stage := evidence.Stages[0]
		if stage.StartedAt.Before(before) || stage.CompletedAt.Before(stage.StartedAt) || stage.CompletedAt.After(time.Now()) {
			t.Fatal("stage timing does not bound its execution")
		}
		if stage.FailureKind != test.kind || (stage.Status == statusPassed) != (test.cause == nil) {
			t.Fatalf("failure evidence = %+v", stage)
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"secret-token", "private-project", "private-api"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("evidence exposed %q", forbidden)
			}
		}
	}
}

func TestNetworkRequestEvidenceIsConcurrentAndBounded(t *testing.T) {
	evidence := &networkEvidence{}
	request := clients.RequestObservation{Service: clients.RequestServiceNeutron, Method: clients.RequestMethodDelete, StatusClass: clients.RequestStatusClassSuccessful, Duration: time.Millisecond}
	var writers sync.WaitGroup
	for range 4 {
		writers.Go(func() {
			for range maximumNetworkRequestEvidence/4 + 1 {
				evidence.ObserveRequest(request)
			}
		})
	}
	if _, err := json.Marshal(evidence); err != nil {
		t.Fatal(err)
	}
	writers.Wait()
	if len(evidence.Requests) != maximumNetworkRequestEvidence || evidence.DroppedRequests != 4 {
		t.Fatalf("request evidence was not bounded: recorded %d, dropped %d", len(evidence.Requests), evidence.DroppedRequests)
	}
	first := evidence.Requests[0]
	if first.Service != request.Service || first.Method != request.Method || first.StatusClass != request.StatusClass || first.DurationSeconds != 0.001 || first.CompletedAt.IsZero() {
		t.Fatalf("request evidence lost timing or bounded classifications: %+v", first)
	}
}
