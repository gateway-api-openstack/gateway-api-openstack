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
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
)

// LoadBalancer includes the custom VIP security groups introduced in API 2.29.
// Remove the extension when Gophercloud exposes vip_sg_ids in its typed model.
type LoadBalancer struct {
	loadbalancers.LoadBalancer
	VIPSecurityGroupIDs []string `json:"vip_sg_ids"`
}

// UnmarshalJSON preserves Gophercloud's timestamp handling and the API extension.
func (lb *LoadBalancer) UnmarshalJSON(data []byte) error {
	var base loadbalancers.LoadBalancer
	if err := json.Unmarshal(data, &base); err != nil {
		return err
	}
	var extra struct {
		VIPSecurityGroupIDs []string `json:"vip_sg_ids"`
	}
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	lb.LoadBalancer, lb.VIPSecurityGroupIDs = base, extra.VIPSecurityGroupIDs
	return nil
}

// CreateLoadBalancerOpts extends the SDK request with custom VIP security groups.
type CreateLoadBalancerOpts struct {
	loadbalancers.CreateOpts
	VIPSecurityGroupIDs []string
}

// ToLoadBalancerCreateMap adds vip_sg_ids only when the class references groups.
func (opts CreateLoadBalancerOpts) ToLoadBalancerCreateMap() (map[string]any, error) {
	body, err := opts.CreateOpts.ToLoadBalancerCreateMap()
	if err != nil {
		return nil, err
	}
	if len(opts.VIPSecurityGroupIDs) > 0 {
		body["loadbalancer"].(map[string]any)["vip_sg_ids"] = opts.VIPSecurityGroupIDs
	}
	return body, nil
}

// ListLoadBalancers lists and extracts load balancers matching opts.
func (s *Service) ListLoadBalancers(ctx context.Context, opts loadbalancers.ListOpts) ([]LoadBalancer, error) {
	pages, err := loadbalancers.List(s.client, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list load balancers: %w", err)
	}
	var result struct {
		LoadBalancers []LoadBalancer `json:"loadbalancers"`
	}
	if err := pages.(loadbalancers.LoadBalancerPage).ExtractInto(&result); err != nil {
		return nil, fmt.Errorf("extract load balancers: %w", err)
	}
	return result.LoadBalancers, nil
}

// GetLoadBalancer gets one load balancer by ID.
func (s *Service) GetLoadBalancer(ctx context.Context, id string) (*LoadBalancer, error) {
	var result struct {
		LoadBalancer *LoadBalancer `json:"loadbalancer"`
	}
	err := loadbalancers.Get(ctx, s.client, id).ExtractInto(&result)
	return result.LoadBalancer, err
}

// CreateLoadBalancer creates one load balancer.
func (s *Service) CreateLoadBalancer(ctx context.Context, opts CreateLoadBalancerOpts) (*loadbalancers.LoadBalancer, error) {
	return loadbalancers.Create(ctx, s.client, opts).Extract()
}

// UpdateLoadBalancer updates one load balancer.
func (s *Service) UpdateLoadBalancer(ctx context.Context, id string, opts loadbalancers.UpdateOpts) (*loadbalancers.LoadBalancer, error) {
	return loadbalancers.Update(ctx, s.client, id, opts).Extract()
}

// DeleteLoadBalancer deletes one load balancer without cascade options.
func (s *Service) DeleteLoadBalancer(ctx context.Context, id string) error {
	return loadbalancers.Delete(ctx, s.client, id, nil).ExtractErr()
}
