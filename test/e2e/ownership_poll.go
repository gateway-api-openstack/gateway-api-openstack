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
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// waitForOwnershipInventory retries only an explicitly incomplete inventory.
// Audit command failures and invalid ownership evidence stop the check instead
// of being hidden until the suite deadline. A timeout retains the last reason.
func waitForOwnershipInventory(ctx context.Context, interval time.Duration, observe func(context.Context) error) error {
	var pending error
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		err := observe(ctx)
		if inventoryPending(err) {
			pending = err
			return false, nil
		}
		pending = nil
		return err == nil, err
	})
	return errors.Join(err, pending)
}
