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

package octavia

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
)

func TestLoadBalancerExtractionPreservesVIPSecurityGroups(t *testing.T) {
	requests := 0
	item := map[string]any{
		"id": "lb-1", "project_id": "project-a", "vip_sg_ids": []string{"sg-a", "sg-b"},
		"tags": []string{"owned"}, "created_at": "2026-10-01T09:00:00", "updated_at": "2026-10-01T10:00:00",
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet {
			t.Fatalf("observation attempted %s", request.Method)
		}
		var body map[string]any
		switch request.URL.Path {
		case "/lbaas/loadbalancers":
			if request.URL.Query().Get("project_id") != "project-a" {
				t.Fatal("missing project filter")
			}
			body = map[string]any{"loadbalancers": []any{item}}
		case "/lbaas/loadbalancers/lb-1":
			body = map[string]any{"loadbalancer": item}
		default:
			t.Fatalf("unexpected request %s", request.URL)
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatal(err)
		}
	})
	service := NewService(&gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: loadBalancerTestTransport{handler}}}, Endpoint: "http://octavia.test/",
	})
	listed, err := service.ListLoadBalancers(context.Background(), loadbalancers.ListOpts{ProjectID: "project-a"})
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %#v, %v", listed, err)
	}
	fetched, err := service.GetLoadBalancer(context.Background(), "lb-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, lb := range []*LoadBalancer{&listed[0], fetched} {
		if lb.ID != "lb-1" || lb.ProjectID != "project-a" || !reflect.DeepEqual(lb.VIPSecurityGroupIDs, []string{"sg-a", "sg-b"}) ||
			!reflect.DeepEqual(lb.Tags, []string{"owned"}) || lb.CreatedAt.IsZero() || lb.UpdatedAt.IsZero() {
			t.Fatalf("extraction dropped identity, timestamp, or security groups: %#v", lb)
		}
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want one list and one get without extension fetches", requests)
	}
}

func TestCreateLoadBalancerIncludesOnlyReferencedVIPSecurityGroups(t *testing.T) {
	for _, groups := range [][]string{nil, {"sg-a", "sg-b"}} {
		opts := CreateLoadBalancerOpts{
			CreateOpts:          loadbalancers.CreateOpts{Name: "test-gateway", VipSubnetID: "subnet-1", Provider: "amphora"},
			VIPSecurityGroupIDs: groups,
		}
		body, err := opts.ToLoadBalancerCreateMap()
		if err != nil {
			t.Fatal(err)
		}
		lb := body["loadbalancer"].(map[string]any)
		value, present := lb["vip_sg_ids"]
		if present != (len(groups) > 0) || present && !reflect.DeepEqual(value, groups) {
			t.Fatalf("vip_sg_ids = %#v, present %t, want %#v", value, present, groups)
		}
		if lb["vip_subnet_id"] != "subnet-1" || lb["provider"] != "amphora" {
			t.Fatalf("extension changed SDK fields: %#v", lb)
		}
	}
}

type loadBalancerTestTransport struct{ handler http.Handler }

func (transport loadBalancerTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, request)
	result := response.Result()
	result.Request = request
	return result, nil
}
