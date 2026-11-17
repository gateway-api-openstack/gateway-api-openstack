# ADR 0007: Direct endpoint members

Status: Accepted for development

Date: 2026-11-10

The repository owner requested local implementation of direct endpoint forwarding, with network verification deferred until a suitable development environment is available.
This decision extends ADR 0002 and the forwarding direction in ADR 0006.
It does not establish a supported cloud environment or authorize network changes.

## Decision

Add `memberMode` to OpenStackGatewayClassConfig with `NodePort` as its default and `Endpoint` as an explicit development opt-in.
Freeze the resolved mode in the existing Gateway topology binding before provisioning.
Changing a class does not change the forwarding mode of a bound Gateway.
Do not add a second runtime gate or a fallback that changes user Services.

New bindings record the member mode explicitly.
This pre-alpha change does not convert older bindings.
Finish cleanup of existing graphs with their original controller before deploying this revision.

The initial Endpoint mode uses IPv4 addresses reachable from the selected existing member subnet.
The member address need not belong to that subnet's CIDR.
Require IPv4 VIP and member subnets and retain the existing project and Amphora capability checks.
The operator must establish forward routing, return routing, port security, and network policy before using this mode in a cloud.
No specific CNI or deployment is verified by this implementation.
Unknown connectivity remains unknown in diagnostics, and the compatibility documentation must not list this mode as verified until live tests pass.

Resolve ClusterIP, headless, and NodePort Services through their selected Service port and EndpointSlices.
Use the Service namespace and service-name label, including manually managed slices without owner references.
Use each slice's resolved port matching the Service port name.
Reject the referenced Service's ClusterIP addresses as endpoint destinations and reject LoadBalancer or ExternalName Services.
Node address selection, worker eligibility, and Service traffic policies do not select endpoint members.
Endpoint mode initially requires unmanaged backend security because a Pod address cannot be mapped to a worker Neutron port using the NodePort identity checks.
Referenced frontend security remains available.

Select ready, serving, nonterminating IPv4 endpoints for new traffic.
Keep serving terminating endpoints as members with weight zero while they remain in EndpointSlices, then remove them when they stop serving or disappear.
This requests Octavia's draining behavior without promising uninterrupted connections or a drain timeout.
Conflicting duplicate observations must not restore traffic to an endpoint that another observation marks unavailable or terminating.
Canonicalize and deduplicate addresses and ports within the single backend and keep the result deterministic.
Reject unsafe destination addresses rather than sending traffic to loopback, link-local, multicast, or the selected Service's virtual addresses.

A valid Service with no eligible endpoints retains its owned pool, monitor, and route policies while removing its members.
It does not become an invalid reference or trigger route graph replacement.
This does not implement the separate Core HTTP response requirements for invalid or omitted backends.
Reconcile only membership differences and draining changes, preserving unchanged member identities.
Existing immutable ownership validation, per-Gateway serialization, asynchronous operation ordering, and cleanup remain in force.

## Verification

Local tests cover API validation, frozen mode, Service and EndpointSlice resolution, condition changes, duplicate observations, draining, empty membership, event handling, and ordered provider mutations.
They must also preserve NodePort behavior and prove that Endpoint mode does not read Nodes or mutate external network resources.

Live verification remains required for forward and return traffic, endpoint replacement, removal, draining, named target ports, manual EndpointSlices, and cleanup.
It must use a separately approved network environment before compatibility or conformance claims change.
IPv6, automatic network configuration, managed backend security, multiple backend weighting, and broader listener or route mapping remain outside this decision.

## References

- [Octavia member routing contract](https://docs.openstack.org/api-ref/load-balancer/v2/index.html#create-member).
- [Kuryr Amphora networking](https://docs.openstack.org/kuryr-kubernetes/latest/installation/services.html#default-configuration).
