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
	"context"
	"fmt"
	"net/http"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
)

func (p *Provider) associateGatewayFloatingIP(ctx context.Context, actual observedGatewayGraph) error {
	// Conditional updates prevent a concurrent reassociation from being
	// overwritten after the complete graph was observed and validated.
	for _, alias := range []string{"standard-attr-revisions", "revision-if-match"} {
		supported, err := p.neutron.HasExtension(ctx, alias)
		if err != nil {
			return err
		}
		if !supported {
			return cloud.NewProviderError(cloud.ErrorCategoryDependencyNotReady,
				fmt.Errorf("floating IP recovery requires Neutron extension %s", alias))
		}
	}
	_, err := p.neutron.UpdateFloatingIP(ctx, actual.floatingIP.ID, floatingips.UpdateOpts{
		PortID: &actual.state.VIPPortID, FixedIP: actual.state.VIPAddress, RevisionNumber: &actual.floatingIP.RevisionNumber,
	})
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusPreconditionFailed) || isNotFound(err) {
			err = cloud.NewProviderError(cloud.ErrorCategoryRetryableService, err)
		}
		return fmt.Errorf("associate Gateway Floating IP: %w", err)
	}
	return nil
}
