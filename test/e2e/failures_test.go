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
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/auditexec"
)

func TestCheckFailurePreservesCauseWithoutPublishingDetails(t *testing.T) {
	private := errors.New("secret-token project-id https://private-api.example.test")
	for _, test := range []struct {
		name  string
		cause error
		want  string
	}{
		{name: "unclassified", cause: private, want: checkSummaryFailed},
		{name: "authentication", cause: cloud.NewProviderError(cloud.ErrorCategoryAuthentication, private), want: checkSummaryAuthentication},
		{name: "Kubernetes authentication", cause: apierrors.NewUnauthorized(private.Error()), want: checkSummaryAuthentication},
		{name: "authorization", cause: cloud.NewProviderError(cloud.ErrorCategoryAuthorization, private), want: checkSummaryAuthorization},
		{name: "quota", cause: cloud.NewProviderError(cloud.ErrorCategoryQuota, private), want: checkSummaryQuota},
		{name: "rate limit", cause: cloud.NewProviderError(cloud.ErrorCategoryRateLimit, private), want: checkSummaryRateLimit},
		{name: "timeout", cause: fmt.Errorf("private-operation: %w", context.DeadlineExceeded), want: checkSummaryTimeout},
		{name: "ownership", cause: cloud.NewProviderError(cloud.ErrorCategoryOwnershipConflict, private), want: checkSummaryOwnership},
		{name: "audit command", cause: errors.Join(auditexec.ErrCommand, private), want: checkSummaryAuditCommand},
		{name: "audit invalid", cause: errors.Join(auditexec.ErrInvalidReport, private), want: checkSummaryAuditInvalid},
		{name: "audit findings", cause: errors.Join(auditexec.ErrFindings, private), want: checkSummaryAuditFindings},
		{name: "pending", cause: errInventoryPending, want: checkSummaryInventoryPending},
		{name: "pending timeout", cause: errors.Join(context.DeadlineExceeded, errInventoryPending), want: checkSummaryInventoryTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := newE2EReport(time.Now(), reportTestConfig(projectModeDedicated))
			err := report.failCheck(checkGateway, fmt.Errorf("unsafe operation: %w", test.cause))
			if !errors.Is(err, test.cause) {
				t.Fatal("failure discarded its cause")
			}
			if got := err.Error(); got != "Gateway status: "+test.want {
				t.Fatalf("safe failure = %q, want category %q", got, test.want)
			}
			encoded, marshalErr := json.Marshal(report)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, output := range []string{err.Error(), string(encoded), string(renderMarkdownReport(report))} {
				for _, forbidden := range []string{"secret-token", "project-id", "private-api", "private-operation", "unsafe operation"} {
					if strings.Contains(output, forbidden) {
						t.Fatalf("failure leaked %q", forbidden)
					}
				}
				if !strings.Contains(output, test.want) {
					t.Fatalf("failure category missing from output: %q", output)
				}
			}
		})
	}
}

func TestOwnershipInventoryPollingRetriesOnlyPendingObservations(t *testing.T) {
	for _, pending := range []error{errInventoryPending, auditexec.ErrScopeNotEmpty} {
		calls := 0
		err := waitForOwnershipInventory(t.Context(), time.Millisecond, func(context.Context) error {
			calls++
			if calls < 3 {
				return pending
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("pending observation calls=%d, err=%v", calls, err)
		}
	}
	for _, terminal := range []error{auditexec.ErrCommand, auditexec.ErrInvalidReport, auditexec.ErrFindings, cloud.ErrAuthentication} {
		calls := 0
		err := waitForOwnershipInventory(t.Context(), time.Millisecond, func(context.Context) error {
			calls++
			return terminal
		})
		if !errors.Is(err, terminal) || calls != 1 {
			t.Fatalf("terminal observation calls=%d, err=%v", calls, err)
		}
	}
}

func TestOwnershipInventoryTimeoutRetainsLastObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	last := fmt.Errorf("remaining private-resource-id: %w", auditexec.ErrScopeNotEmpty)
	err := waitForOwnershipInventory(ctx, time.Millisecond, func(context.Context) error { return last })
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, last) {
		t.Fatalf("timeout did not retain its cause and last observation: %v", err)
	}
	if got := failureSummary(err); got != checkSummaryInventoryTimeout {
		t.Fatalf("timeout summary = %q", got)
	}
}

func TestFloatingIPDeletionFailureReportsOnlySafeStageAndCategory(t *testing.T) {
	cause := fmt.Errorf("secret-token project-id https://private-api.example.test: %w", context.DeadlineExceeded)
	for _, test := range []struct {
		stage  floatingIPDeletionStage
		prefix string
	}{
		{stage: floatingIPCloudObservation, prefix: floatingIPCloudFailurePrefix},
		{stage: floatingIPWorkloadRecovery, prefix: floatingIPWorkloadFailurePrefix},
	} {
		config := reportTestConfig(projectModeDedicated)
		config.FloatingIPDeletion = true
		report := newE2EReport(time.Now(), config)
		failure := &floatingIPDeletionFailure{stage: test.stage, cause: cause}
		err := report.failCheck(checkFloatingIPDeletion, fmt.Errorf("private-operation: %w", failure))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("stage error lost the original timeout")
		}
		encoded, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, output := range []string{err.Error(), string(encoded), string(renderMarkdownReport(report))} {
			if !strings.Contains(output, test.prefix+checkSummaryTimeout) {
				t.Fatalf("missing safe failure stage: %s", output)
			}
			for _, forbidden := range []string{"secret-token", "project-id", "private-api", "private-operation"} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("staged failure exposed %q", forbidden)
				}
			}
		}
		if err := report.setCheck(checkGateway, statusFailed, test.prefix+checkSummaryTimeout); err == nil {
			t.Fatal("Floating IP deletion stage was accepted for an unrelated check")
		}
		for _, summary := range []string{test.prefix + cause.Error(), test.prefix + test.prefix + checkSummaryTimeout} {
			if err := report.setCheck(checkFloatingIPDeletion, statusFailed, summary); err == nil {
				t.Fatal("unsafe staged summary was accepted")
			}
		}
	}
}
