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
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/auditexec"
)

const (
	checkSummaryAuthentication   = "Authentication failed"
	checkSummaryAuthorization    = "A required API or file permission was denied"
	checkSummaryRateLimit        = "An API request was rate limited"
	checkSummaryQuota            = "An OpenStack quota was exceeded"
	checkSummaryTimeout          = "An operation exceeded its deadline"
	checkSummaryCanceled         = "The operation was canceled"
	checkSummaryUnavailable      = "An API or network connection was unavailable"
	checkSummaryOwnership        = "Cloud resource ownership could not be verified"
	checkSummaryAuditCommand     = "The ownership audit command failed before returning a report"
	checkSummaryAuditInvalid     = "The ownership audit returned an invalid or incomplete report"
	checkSummaryAuditFindings    = "The ownership audit reported unresolved findings"
	checkSummaryInventoryPending = "The expected ownership inventory has not converged"
	checkSummaryInventoryTimeout = "The expected ownership inventory did not converge before the deadline"
)

var errInventoryPending = errors.New("ownership inventory has not converged")

// checkFailure retains the cause for classification while exposing only the
// known check name and an allowlisted summary to logs and reports.
type checkFailure struct {
	name  string
	cause error
}

func (e *checkFailure) Error() string { return fmt.Sprintf("%s: %s", e.name, failureSummary(e.cause)) }
func (e *checkFailure) Unwrap() error { return e.cause }

func (r *e2eReport) failCheck(name string, err error) error {
	if setErr := r.setCheck(name, statusFailed, failureSummary(err)); setErr != nil {
		panic(setErr)
	}
	return &checkFailure{name: name, cause: err}
}

func failureSummary(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) && inventoryPending(err):
		return checkSummaryInventoryTimeout
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, cloud.ErrTimeout), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return checkSummaryTimeout
	case errors.Is(err, context.Canceled):
		return checkSummaryCanceled
	case errors.Is(err, cloud.ErrAuthentication), apierrors.IsUnauthorized(err), gophercloud.ResponseCodeIs(err, http.StatusUnauthorized):
		return checkSummaryAuthentication
	case errors.Is(err, cloud.ErrAuthorization), apierrors.IsForbidden(err), gophercloud.ResponseCodeIs(err, http.StatusForbidden), errors.Is(err, os.ErrPermission):
		return checkSummaryAuthorization
	case errors.Is(err, cloud.ErrRateLimited), apierrors.IsTooManyRequests(err), gophercloud.ResponseCodeIs(err, http.StatusTooManyRequests):
		return checkSummaryRateLimit
	case errors.Is(err, cloud.ErrQuotaExceeded):
		return checkSummaryQuota
	case errors.Is(err, cloud.ErrOwnershipConflict):
		return checkSummaryOwnership
	case errors.Is(err, auditexec.ErrCommand):
		return checkSummaryAuditCommand
	case errors.Is(err, auditexec.ErrInvalidReport):
		return checkSummaryAuditInvalid
	case errors.Is(err, auditexec.ErrFindings):
		return checkSummaryAuditFindings
	case inventoryPending(err):
		return checkSummaryInventoryPending
	case errors.Is(err, cloud.ErrRetryableService), apierrors.IsServiceUnavailable(err):
		return checkSummaryUnavailable
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return checkSummaryTimeout
		}
		return checkSummaryUnavailable
	}
	return checkSummaryFailed
}

func inventoryPending(err error) bool {
	return errors.Is(err, errInventoryPending) || errors.Is(err, auditexec.ErrScopeNotEmpty)
}
