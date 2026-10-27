//go:build e2e

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
	"fmt"
	"net/http"
	"os"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/audit"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack"
	cloudidentity "github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/identity"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/neutron"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/octavia"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/topology"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/cloudauth"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
)

func (s *openStackSuite) networkClients(ctx context.Context) (*openstack.ServiceClients, error) {
	if s.cloudClients != nil {
		return s.cloudClients, nil
	}
	contents, err := os.ReadFile(s.config.ControllerCloudsYAML)
	if err != nil {
		return nil, fmt.Errorf("read controller credential for network checks: %w", err)
	}
	options := cloudauth.Options{CloudName: s.config.Audit.Cloud, Region: s.config.Audit.Region, Microversion: s.config.Audit.Microversion}
	err = cloudauth.VerifyProjectWithExactCopy(ctx, contents, s.config.Project.ExpectedProjectID, options, func(path string) error {
		clients, err := openstack.NewServiceClients(ctx, openstack.ClientConfig{
			CloudsYAMLPath: path, CloudName: options.CloudName, Region: options.Region, Microversion: options.Microversion,
			APIQPS: openstack.DefaultAPIQPS, APIBurst: openstack.DefaultAPIBurst,
		})
		if err != nil {
			return err
		}
		if clients.ProjectID != s.config.Project.ExpectedProjectID {
			return fmt.Errorf("network check credential changed project during authentication")
		}
		s.cloudClients = &clients
		return nil
	})
	return s.cloudClients, err
}

func (s *openStackSuite) testGatewayNetwork(ctx context.Context, clients *openstack.ServiceClients) (cloudidentity.Identity, cloud.GatewayState, cloud.Topology, error) {
	if err := s.verifyRunScopedControllerClasses(ctx); err != nil {
		return cloudidentity.Identity{}, cloud.GatewayState{}, cloud.Topology{}, err
	}
	gateway, resolved, err := s.readGatewayTopology(ctx)
	if err != nil {
		return cloudidentity.Identity{}, cloud.GatewayState{}, cloud.Topology{}, err
	}
	config := controller.Config{ControllerName: gatewayv1.GatewayController(s.config.ControllerName), ClusterID: s.config.Audit.ClusterID, OpenStackProjectID: s.config.Project.ExpectedProjectID}
	if err := controller.ValidateGatewayBinding(config, gateway); err != nil {
		return cloudidentity.Identity{}, cloud.GatewayState{}, cloud.Topology{}, err
	}
	if !slices.Contains(gateway.Finalizers, config.GatewayFinalizer()) || clients.ProjectID != config.OpenStackProjectID {
		return cloudidentity.Identity{}, cloud.GatewayState{}, cloud.Topology{}, fmt.Errorf("test Gateway lacks its durable ownership boundary")
	}
	identity, err := cloudidentity.NewIdentity(controller.GatewayIdentity(config, gateway))
	if err != nil {
		return cloudidentity.Identity{}, cloud.GatewayState{}, cloud.Topology{}, err
	}
	provider := openstack.NewProvider(*clients, openstack.ProviderConfig{OperationTimeout: s.config.HTTPTimeout})
	result, found, err := provider.GetGateway(ctx, identity.Value())
	if err != nil || !found || result.Outcome.State != cloud.OutcomeReady {
		return identity, cloud.GatewayState{}, cloud.Topology{}, fmt.Errorf("test Gateway cloud graph is not ready for a network check")
	}
	return identity, result.State, resolved.Topology, nil
}

func (s *openStackSuite) verifyFloatingIPRecovery(ctx context.Context) error {
	// This flag authorizes one fault against the existing test-owned address.
	// The suite never allocates another address or changes a shared network.
	if !s.config.FloatingIPRecovery || s.config.Project.Mode != projectModeDedicated || s.config.Project.ExpectedExternalNetworkID == "" {
		return fmt.Errorf("owned Floating IP detachment was not selected for this run")
	}
	clients, err := s.networkClients(ctx)
	if err != nil {
		return err
	}
	identity, state, resolved, err := s.testGatewayNetwork(ctx, clients)
	if err != nil {
		return err
	}
	if state.FloatingIPID == "" || state.FloatingIPAddress == "" || resolved.ExternalNetworkID != s.config.Project.ExpectedExternalNetworkID {
		return fmt.Errorf("test Gateway does not have the approved Floating IP network")
	}
	if err := detachTestFloatingIP(ctx, clients.Network, identity, state, resolved.ExternalNetworkID); err != nil {
		return err
	}
	if err := s.triggerConvergedRouteObservation(ctx); err != nil {
		return err
	}
	if err := wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		observed, err := floatingips.Get(ctx, clients.Network, state.FloatingIPID).Extract()
		if err != nil {
			return false, err
		}
		if observed.PortID == "" {
			return false, nil
		}
		return true, validateTestFloatingIP(observed, identity, state, resolved.ExternalNetworkID)
	}); err != nil {
		return fmt.Errorf("wait for the original Floating IP association: %w", err)
	}
	if err := s.verifyWorkloadAfterRecovery(ctx); err != nil {
		return err
	}
	_, err = s.waitForActiveOwnershipInventory(ctx)
	return err
}

func detachTestFloatingIP(ctx context.Context, client *gophercloud.ServiceClient, identity cloudidentity.Identity, state cloud.GatewayState, networkID string) error {
	address, err := readTestFloatingIPForFault(ctx, client, identity, state, networkID)
	if err != nil {
		return err
	}
	detached := ""
	_, err = neutron.NewService(client).UpdateFloatingIP(ctx, address.ID, floatingips.UpdateOpts{PortID: &detached, RevisionNumber: &address.RevisionNumber})
	if err != nil {
		return fmt.Errorf("detach owned test Floating IP: %w", err)
	}
	return nil
}

func deleteTestFloatingIP(ctx context.Context, client *gophercloud.ServiceClient, identity cloudidentity.Identity, state cloud.GatewayState, networkID string) error {
	address, err := readTestFloatingIPForFault(ctx, client, identity, state, networkID)
	if err != nil {
		return err
	}
	// Neutron revision-if-match constrains DELETE as well as UPDATE. The SDK's
	// Floating IP Delete helper has no request options, so keep the conditional
	// request here rather than weakening the fault's concurrency protection.
	_, err = client.Delete(ctx, client.ServiceURL("floatingips", address.ID), &gophercloud.RequestOpts{
		OkCodes:     []int{http.StatusNoContent},
		MoreHeaders: map[string]string{"If-Match": fmt.Sprintf("revision_number=%d", address.RevisionNumber)},
	})
	if err != nil {
		return fmt.Errorf("delete owned test Floating IP: %w", err)
	}
	return nil
}

func readTestFloatingIPForFault(ctx context.Context, client *gophercloud.ServiceClient, identity cloudidentity.Identity, state cloud.GatewayState, networkID string) (*floatingips.FloatingIP, error) {
	network := neutron.NewService(client)
	for _, alias := range []string{"standard-attr-revisions", "revision-if-match"} {
		supported, err := network.HasExtension(ctx, alias)
		if err != nil || !supported {
			return nil, fmt.Errorf("floating IP fault requires conditional Neutron updates and deletion")
		}
	}
	// Re-read the exact address immediately before the conditional mutation.
	// Cached IDs and the advisory audit only locate candidates.
	address, err := floatingips.Get(ctx, client, state.FloatingIPID).Extract()
	if err != nil {
		return nil, fmt.Errorf("read test Floating IP before fault injection: %w", err)
	}
	if err := validateTestFloatingIP(address, identity, state, networkID); err != nil {
		return nil, err
	}
	return address, nil
}

// verifyFloatingIPDeletion releases the test address before permitting its
// controller to allocate a replacement. It never retains two addresses at once.
func (s *openStackSuite) verifyFloatingIPDeletion(ctx context.Context) error {
	if !s.config.FloatingIPDeletion || s.config.Project.Mode != projectModeDedicated || s.config.Project.ExpectedExternalNetworkID == "" {
		return fmt.Errorf("owned Floating IP deletion was not selected for this run")
	}
	clients, err := s.networkClients(ctx)
	if err != nil {
		return err
	}
	identity, state, resolved, err := s.testGatewayNetwork(ctx, clients)
	if err != nil {
		return err
	}
	if state.FloatingIPID == "" || resolved.ExternalNetworkID != s.config.Project.ExpectedExternalNetworkID {
		return fmt.Errorf("test Gateway does not have the approved Floating IP network")
	}
	before, err := s.readOwnershipAudit(ctx, false)
	if err != nil {
		return err
	}
	if err := deleteTestFloatingIP(ctx, clients.Network, identity, state, resolved.ExternalNetworkID); err != nil {
		return err
	}
	if _, err := floatingips.Get(ctx, clients.Network, state.FloatingIPID).Extract(); !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return fmt.Errorf("deleted test Floating IP is not confirmed absent")
	}
	provider := openstack.NewProvider(*clients, openstack.ProviderConfig{OperationTimeout: s.config.HTTPTimeout})
	var replacement cloud.GatewayState
	if err := wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		observed, found, err := provider.GetGateway(ctx, identity.Value())
		if err != nil {
			return false, err
		}
		if !found || observed.Outcome.State != cloud.OutcomeReady || observed.State.FloatingIPID == "" {
			return false, nil
		}
		replacement = observed.State
		if replacement.FloatingIPID == state.FloatingIPID || replacement.LoadBalancerID != state.LoadBalancerID || replacement.ListenerID != state.ListenerID ||
			replacement.VIPPortID != state.VIPPortID || replacement.VIPAddress != state.VIPAddress {
			return false, fmt.Errorf("floating IP replacement changed the parent graph or reused the deleted identity")
		}
		address, err := floatingips.Get(ctx, clients.Network, replacement.FloatingIPID).Extract()
		if err != nil {
			return false, err
		}
		return true, validateTestFloatingIP(address, identity, replacement, resolved.ExternalNetworkID)
	}); err != nil {
		return fmt.Errorf("wait for Floating IP replacement through periodic observation: %w", err)
	}
	if err := s.verifyWorkloadAfterRecovery(ctx); err != nil {
		return err
	}
	after, err := s.readOwnershipAudit(ctx, false)
	if err != nil {
		return err
	}
	if err := validateFloatingIPReplacementInventory(before, after, state.FloatingIPID, replacement.FloatingIPID); err != nil {
		return err
	}
	// Later restart checks compare against this fully validated replacement,
	// while the final cleanup check still uses the original empty baseline.
	summary := sanitizeAuditSummary(after)
	s.activeAudit, s.activeAuditFingerprint = &summary, ownershipAuditFingerprint(after.Resources)
	return nil
}

func validateFloatingIPReplacementInventory(before, after audit.Report, oldID, newID string) error {
	withoutAddress := func(resources []audit.ResourceFinding, id string) ([]audit.ResourceFinding, bool) {
		result := make([]audit.ResourceFinding, 0, len(resources))
		count := 0
		for _, resource := range resources {
			if resource.Service == "Neutron" && resource.Type == "FloatingIP" && resource.ID == id {
				count++
				continue
			}
			result = append(result, resource)
		}
		return result, count == 1
	}
	oldResources, oldFound := withoutAddress(before.Resources, oldID)
	newResources, newFound := withoutAddress(after.Resources, newID)
	if oldID == "" || newID == "" || oldID == newID || !oldFound || !newFound ||
		sanitizeAuditSummary(before) != sanitizeAuditSummary(after) || ownershipAuditFingerprint(oldResources) != ownershipAuditFingerprint(newResources) {
		return fmt.Errorf("floating IP replacement did not preserve the remaining ownership inventory")
	}
	return nil
}

func validateTestFloatingIP(address *floatingips.FloatingIP, identity cloudidentity.Identity, state cloud.GatewayState, networkID string) error {
	if address == nil || state.FloatingIPID == "" || state.VIPPortID == "" || state.VIPAddress == "" ||
		address.ID != state.FloatingIPID || address.ProjectID != identity.Value().OpenStackProjectID ||
		address.TenantID != "" && address.TenantID != address.ProjectID || !identity.MatchesGatewayDescription(address.Description, cloudidentity.RoleFloatingIP) ||
		address.PortID != state.VIPPortID || address.FixedIP != state.VIPAddress || address.FloatingIP != state.FloatingIPAddress || address.FloatingNetworkID != networkID {
		return fmt.Errorf("test Floating IP no longer matches its immutable identity and VIP association")
	}
	return nil
}

func (s *openStackSuite) verifyReferencedSecurity(ctx context.Context) error {
	clients, err := s.networkClients(ctx)
	if err != nil {
		return err
	}
	identity, state, resolved, err := s.testGatewayNetwork(ctx, clients)
	if err != nil {
		return err
	}
	if !slices.Equal(resolved.VIPSecurityGroupIDs, s.config.Project.FrontendSecurityGroupIDs) ||
		!slices.Equal(resolved.BackendSecurityGroupIDs, s.config.Project.BackendSecurityGroupIDs) {
		return fmt.Errorf("test Gateway did not freeze the selected security group references")
	}
	lb, err := octavia.NewService(clients.LoadBalancer).GetLoadBalancer(ctx, state.LoadBalancerID)
	if err != nil {
		return err
	}
	slices.Sort(lb.VIPSecurityGroupIDs)
	if lb.ProjectID != clients.ProjectID || !identity.MatchesGateway(lb.Tags, cloudidentity.RoleLoadBalancer) || !slices.Equal(lb.VIPSecurityGroupIDs, resolved.VIPSecurityGroupIDs) {
		return fmt.Errorf("test load balancer does not have the frozen VIP security groups")
	}
	routes, err := s.referencedBackendModels(ctx, clients, identity, resolved)
	if err != nil {
		return err
	}
	return topology.NewResolver(*clients, s.config.HTTPTimeout).ValidateGatewaySecurity(ctx, cloud.GatewaySpec{
		VIPSubnetID: resolved.VIPSubnetID, VIPSecurityGroupIDs: resolved.VIPSecurityGroupIDs, ListenerPort: listenerPort,
	}, routes)
}

func (s *openStackSuite) referencedBackendModels(ctx context.Context, clients *openstack.ServiceClients, gatewayIdentity cloudidentity.Identity, resolved cloud.Topology) ([]cloud.RouteSpec, error) {
	if len(resolved.BackendSecurityGroupIDs) == 0 {
		return nil, nil
	}
	var route gatewayv1.HTTPRoute
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: routeName}, &route); err != nil {
		return nil, err
	}
	if route.UID != s.routeUID || route.Annotations[runAnnotation] != s.config.RunID || !route.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("test HTTPRoute identity changed during security validation")
	}
	id := gatewayIdentity.Value()
	id.RouteNamespace, id.RouteName, id.RouteUID = route.Namespace, route.Name, string(route.UID)
	identity, err := cloudidentity.NewIdentity(id)
	if err != nil {
		return nil, err
	}
	var nodes corev1.NodeList
	if err := s.client.List(ctx, &nodes); err != nil {
		return nil, err
	}
	providers := make(map[string]string)
	for _, node := range nodes.Items {
		if controller.NodePortNodeExcluded(&node) {
			continue
		}
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP {
				if _, duplicate := providers[address.Address]; duplicate {
					return nil, fmt.Errorf("worker address is ambiguous during security validation")
				}
				providers[address.Address] = node.Spec.ProviderID
			}
		}
	}
	report, err := s.readOwnershipAudit(ctx, false)
	if err != nil {
		return nil, err
	}
	model := cloud.RouteSpec{Identity: id, MemberSubnetID: resolved.MemberSubnetID, BackendSecurityGroupIDs: resolved.BackendSecurityGroupIDs}
	for _, resource := range report.Resources {
		if resource.Service != "Octavia" || resource.Type != "Member" {
			continue
		}
		member, err := pools.GetMember(ctx, clients.LoadBalancer, resource.ParentID, resource.ID).Extract()
		if err != nil {
			return nil, err
		}
		providerID := providers[member.Address]
		if member.ID != resource.ID || !identity.MatchesRoute(member.Tags, cloudidentity.RoleMember) || providerID == "" {
			return nil, fmt.Errorf("test member does not map to the expected route and worker")
		}
		model.Members = append(model.Members, cloud.Member{Address: member.Address, Port: member.ProtocolPort, ProviderID: providerID})
	}
	if len(model.Members) == 0 {
		return nil, fmt.Errorf("test route has no observed members for security validation")
	}
	return []cloud.RouteSpec{model}, nil
}
