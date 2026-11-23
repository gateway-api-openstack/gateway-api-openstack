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
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

func TestServiceClientsKeepAuthenticationAndHandlersIndependent(t *testing.T) {
	var requests [2]*http.Request
	var clients [2]*gophercloud.ServiceClient
	for i := range clients {
		clients[i] = ServiceClient(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests[i] = request
			writer.WriteHeader(http.StatusOK)
		}))
	}
	clients[0].TokenID = "first-client-token"
	tokens := []string{"first-client-token", "test-token"}
	for i, client := range clients {
		response, err := client.Get(t.Context(), client.ServiceURL("resources"), nil, nil)
		if err != nil {
			t.Fatalf("client %d request failed: %v", i, err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if requests[i] == nil || response.Request != requests[i] {
			t.Fatalf("client %d lost its handler request", i)
		}
		if got := requests[i].Header.Get("X-Auth-Token"); got != tokens[i] {
			t.Fatalf("client %d token = %q, want %q", i, got, tokens[i])
		}
	}
}
