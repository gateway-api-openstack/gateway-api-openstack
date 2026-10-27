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

package clients

import (
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

func TestKeyManagerClientPreservesEndpointSelection(t *testing.T) {
	var selected gophercloud.EndpointOpts
	provider := &gophercloud.ProviderClient{EndpointLocator: func(options gophercloud.EndpointOpts) (string, error) {
		selected = options
		return "https://barbican.invalid/v1/", nil
	}}
	serviceClients := ServiceClients{
		Network:      &gophercloud.ServiceClient{ProviderClient: provider},
		endpointOpts: gophercloud.EndpointOpts{Region: "test-region", Availability: gophercloud.AvailabilityInternal},
	}
	keyManager, err := serviceClients.NewKeyManagerClient()
	if err != nil {
		t.Fatal(err)
	}
	if selected.Region != "test-region" || selected.Availability != gophercloud.AvailabilityInternal || selected.Type != "key-manager" {
		t.Fatalf("endpoint selection was not preserved: %+v", selected)
	}
	if keyManager.ProviderClient != provider {
		t.Fatal("Barbican did not reuse the authenticated provider and rate limiter")
	}
	if _, err := (ServiceClients{}).NewKeyManagerClient(); err == nil {
		t.Fatal("missing authenticated provider was accepted")
	}
}
