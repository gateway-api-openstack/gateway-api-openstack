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

// Package diagnostics inspects OpenStack prerequisites without mutating resources.
package diagnostics

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/apierrors"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/topology"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/diagnostics"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/quotas"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

type topologyReader interface {
	cloud.TopologyResolver
	ValidateMemberPort(context.Context, string, string, string, []string) error
}

// Scanner reuses authenticated, rate-limited clients and the controller's
// topology validation. It never creates an object to test write permissions.
type Scanner struct {
	clients  clients.ServiceClients
	topology topologyReader
	timeout  time.Duration
}

// NewScanner creates a scanner with a deadline shared by its checks.
func NewScanner(serviceClients clients.ServiceClients, timeout time.Duration) *Scanner {
	return &Scanner{
		clients: serviceClients, timeout: timeout,
		topology: topology.NewResolver(serviceClients, timeout),
	}
}

var _ diagnostics.Inspector = (*Scanner)(nil)

// Inspect reports structural prerequisites independently. An unknown result
// cannot establish connectivity, a write permission, or future quota availability.
func (s *Scanner) Inspect(ctx context.Context, request diagnostics.Request) diagnostics.Report {
	report := diagnostics.NewReport()
	if s.clients.ProjectID == "" || s.clients.Network == nil || s.clients.LoadBalancer == nil || s.timeout <= 0 {
		appendCheck(&report, "authentication", diagnostics.Failed, "Project-scoped service clients and a positive timeout are required.")
		return report
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	appendCheck(&report, "authentication", diagnostics.Passed, "Authentication is scoped to one OpenStack project.")

	resolved, err := s.topology.ResolveTopology(ctx, request.Topology)
	if err != nil {
		appendCheck(&report, "topology", diagnostics.Failed, failedRead("Topology and Amphora capability validation failed", err))
	} else {
		appendCheck(&report, "topology", diagnostics.Passed, "Amphora, the configured Octavia API version, and the selected infrastructure are available.")
	}
	s.inspectNodes(ctx, request.Nodes, resolved, err == nil, &report)
	s.inspectSecurityGroups(ctx, &report)
	appendCheck(&report, "securityGroupUpdate", diagnostics.Unknown, "Read APIs cannot prove permission to update security groups or worker ports.")
	appendCheck(&report, "backendReachability", diagnostics.Unknown, "Port structure does not prove Amphora source routing, NodePort access, or healthy traffic.")
	appendCheck(&report, "dataPlaneTopology", diagnostics.Unknown, "Amphora topology and the kube-proxy implementation require environment-specific evidence.")
	if request.Topology.ExternalNetwork == nil {
		appendCheck(&report, "floatingIPQuota", diagnostics.Skipped, "The configuration does not allocate a Floating IP.")
	} else {
		s.inspectFloatingIPQuota(ctx, &report)
	}
	if request.CheckBarbican {
		s.inspectBarbican(ctx, &report)
	} else {
		appendCheck(&report, "barbican", diagnostics.Skipped, "Barbican metadata access was not requested.")
	}
	return report
}

func (s *Scanner) inspectNodes(ctx context.Context, nodes []diagnostics.Node, resolved cloud.Topology, topologyReady bool, report *diagnostics.Report) {
	if len(nodes) == 0 {
		appendCheck(report, "nodePort", diagnostics.Unknown, "No selected worker Nodes were available for port validation.")
		return
	}
	for index, node := range nodes {
		check := diagnostics.Check{Name: "nodePort", Node: index + 1}
		if !topologyReady {
			check.Status, check.Message = diagnostics.Skipped, "Port validation requires resolved topology."
		} else if err := s.topology.ValidateMemberPort(ctx, node.ProviderID, node.Address, resolved.MemberSubnetID, resolved.BackendSecurityGroupIDs); err != nil {
			check.Status, check.Message = diagnostics.Failed, failedRead("Worker port prerequisites could not be validated", err)
		} else {
			check.Status, check.Message = diagnostics.Passed, "One port matches the project, server, selected address, subnet, and selected backend security policy."
		}
		report.Checks = append(report.Checks, check)
		if address, err := netip.ParseAddr(node.Address); err == nil {
			family := "IPv6"
			if address.Unmap().Is4() {
				family = "IPv4"
			}
			if !slices.Contains(report.IPFamilies, family) {
				report.IPFamilies = append(report.IPFamilies, family)
			}
		}
	}
	slices.Sort(report.IPFamilies)
}

func (s *Scanner) inspectSecurityGroups(ctx context.Context, report *diagnostics.Report) {
	// One page proves list visibility. The report does not inventory groups or
	// retain any returned name, rule, or identifier.
	err := groups.List(s.clients.Network, groups.ListOpts{ProjectID: s.clients.ProjectID, Limit: 1}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		_, err := groups.ExtractGroups(page)
		return false, err
	})
	if err != nil {
		appendCheck(report, "securityGroupRead", diagnostics.Failed, failedRead("Security group metadata could not be read", err))
		return
	}
	appendCheck(report, "securityGroupRead", diagnostics.Passed, "The credential can list security group metadata in its project.")
}

func (s *Scanner) inspectFloatingIPQuota(ctx context.Context, report *diagnostics.Report) {
	quota, err := quotas.GetDetail(ctx, s.clients.Network, s.clients.ProjectID).Extract()
	if err != nil {
		appendCheck(report, "floatingIPQuota", diagnostics.Unknown, failedRead("Floating IP quota usage could not be read", err))
		return
	}
	if quota.FloatingIP.Limit >= 0 && quota.FloatingIP.Used+quota.FloatingIP.Reserved >= quota.FloatingIP.Limit {
		appendCheck(report, "floatingIPQuota", diagnostics.Failed, "No Floating IP quota was available at observation time.")
		return
	}
	appendCheck(report, "floatingIPQuota", diagnostics.Passed, "Floating IP quota was available at observation time. This does not reserve capacity or prove allocation permission.")
}

func (s *Scanner) inspectBarbican(ctx context.Context, report *diagnostics.Report) {
	keyManager, err := s.clients.NewKeyManagerClient()
	if err == nil {
		// List metadata only. Payloads, ACL changes, and certificate creation are
		// outside this read-only diagnostic.
		err = secrets.List(keyManager, secrets.ListOpts{Limit: 1}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
			_, err := secrets.ExtractSecrets(page)
			return false, err
		})
	}
	if err != nil {
		appendCheck(report, "barbican", diagnostics.Failed, failedRead("Barbican metadata access could not be validated", err))
		return
	}
	appendCheck(report, "barbican", diagnostics.Passed, "Barbican secret metadata is readable. Payload access, certificate writes, and Octavia access remain unproved.")
}

func appendCheck(report *diagnostics.Report, name string, status diagnostics.Status, message string) {
	report.Checks = append(report.Checks, diagnostics.Check{Name: name, Status: status, Message: message})
}

func failedRead(message string, err error) string {
	if category, known := cloud.ErrorCategoryOf(apierrors.Classify(err)); known {
		return message + " (" + string(category) + ")."
	}
	return message + "."
}
