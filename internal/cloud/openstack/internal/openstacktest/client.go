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

package openstacktest

import (
	"net/http"
	"net/http/httptest"

	"github.com/gophercloud/gophercloud/v2"
)

// ServiceClient returns an independent client that runs handler synchronously
// without opening a network connection.
func ServiceClient(handler http.Handler) *gophercloud.ServiceClient {
	provider := &gophercloud.ProviderClient{TokenID: "test-token"}
	provider.HTTPClient.Transport = handlerTransport{handler: handler}
	return &gophercloud.ServiceClient{
		ProviderClient: provider,
		Endpoint:       "http://openstack.test/",
	}
}

type handlerTransport struct {
	handler http.Handler
}

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, request)
	result := response.Result()
	result.Request = request
	return result, nil
}
