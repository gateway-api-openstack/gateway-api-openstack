# ADR 0004: Network access and address recovery

Status: Accepted for the Phase 4 development implementation

Date: 2026-10-22

The repository owner requested the Phase 4 implementation without migration compatibility.
This decision covers its initial security modes and owned address recovery.
It does not authorize cloud operations in a shared project or claim community review.

## Decision

### Referenced security groups

Add `frontendSecurity` and `backendSecurity` to OpenStackGatewayClassConfig.
Frontend defaults to `OctaviaManaged`, which omits custom VIP security groups.
Backend defaults to `Unmanaged`, which leaves worker security to the environment.
Both accept `Referenced` with explicit existing security group selectors.
Resolve each selector to exactly one group in the authenticated project and reject duplicate results.
Store sorted group IDs in the existing frozen Gateway topology.
Do not copy modes into the resolved model when the presence of IDs already determines the behavior.

Referenced frontend groups require advertised and configured Octavia API 2.29 or newer.
Use a narrow typed adapter for `vip_sg_ids` where the pinned SDK lacks the field.
Verify the observed group list against the binding before mutation.
Do not use listener `allowed_cidrs` or SR-IOV placement with custom VIP groups.
Validate ingress rule prerequisites before programming a listener, but do not claim that these reads prove reachability.

Referenced backend groups must exist and be attached to each selected worker port.
Identify that port from the Node provider ID, configured region, selected fixed IP, member subnet, authenticated project, compute device owner, and port security state.
Reject ambiguous mappings and never fall back to a Node name or IP alone.
Check TCP ingress rule prerequisites for the selected NodePort and IP family.
Do not infer Amphora source CIDRs from the VIP subnet.
The operator remains responsible for the source policy and actual network reachability.

Read these dependencies again before an Ensure mutation and during the existing slow observation cycle.
Deletion must remain possible when a referenced group or worker disappears.
Referenced groups, rules, and worker ports never become controller-owned resources.

### Owned Floating IP recovery

Rediscover the owned address within the authenticated project and validate the complete Gateway graph before restoring its association.
Recover the same address onto the validated VIP port and fixed IP instead of allocating a replacement.
Require Neutron revision support and a conditional update so a concurrent association change cannot be overwritten.
Do not restore external access while an unresolved preserved route prevents traffic restoration.
Issue at most one mutation per reconcile and observe the association before reporting the address as ready.
Existing or foreign addresses remain outside this controller's ownership.

### Read-only diagnostics

Provide a separate diagnostic command using the controller's authenticated service clients and topology resolver.
Inspect API versions, network placement, security prerequisites, worker port mappings, Floating IP quota, and optional Barbican metadata access.
Do not issue trial mutations or read secret payloads.
Report update authorization and end-to-end reachability as unknown when reads cannot establish them.
Use fixed diagnostic codes and messages without tenant IDs, addresses, resource names, credentials, or raw API errors.

### Unresolved capabilities

The owner explicitly excluded `Managed` backend security from this implementation.
It remains a separate ownership decision because it modifies worker ports owned by another component.
No default, fallback, or migration path may enable it.
Core operations missing from the public Octavia API remain explicitly unsupported.
Small typed compiler entry points may return unsupported errors with TODOs for the required upstream API work, but must not silently accept a configuration or create approximate resources.

## Verification

Tests must cover ambiguous references, foreign projects, missing capabilities, frozen settings, referenced rule and port changes, conditional address recovery, no-op reconciliation, and cleanup when dependencies disappear.
Diagnostic adapter tests must reject every mutating request and verify redaction on failure.
Real traffic, IPv4 and IPv6 compatibility, and conformance remain separate evidence gates.
