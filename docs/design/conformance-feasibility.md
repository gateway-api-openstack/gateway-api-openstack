# GATEWAY-HTTP feasibility on Amphora

This source assessment supports the Phase 4 designs required for GATEWAY-HTTP conformance.
It is not an accepted resource-mapping ADR or live test evidence.

## Target and evidence

The [roadmap](../../ROADMAP.md#phase-5---complete-gateway-http-conformance) requires all Core tests and tests for declared Extended features to pass.
This assessment compares Gateway API v1.6.1 Standard Channel types and tests with [Octavia 19.0.0](https://releases.openstack.org/hibiscus/index.html#octavia), commit `93689cb7701da65b5aad387bb824c477c64b0f2b`.
It covers the public Octavia API and the stock Amphora HAProxy configuration generator.
Reassess these findings when changing either version.

The current direct Amphora architecture cannot satisfy full Core through that API.
These are limits of the exposed Octavia model, not claims that HAProxy itself cannot perform those operations.

## Current API blockers

### Request header modification

Core requires route-level request header add, set, and remove operations.
The pinned [upstream test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/conformance/tests/httproute-request-header-modifier.go) distinguishes this Core filter from the Extended backendRef-level variant.

Octavia's listener `insert_headers` accepts a fixed set of forwarding and TLS headers.
It does not expose arbitrary header names or per-route add, set, and remove actions.
The [listener validation](https://github.com/openstack/octavia/blob/93689cb7701da65b5aad387bb824c477c64b0f2b/octavia/api/v2/controllers/listener.py#L146) and [L7 policy API](https://github.com/openstack/octavia/blob/93689cb7701da65b5aad387bb824c477c64b0f2b/octavia/api/v2/types/l7policy.py) rule out using those fields as a general RequestHeaderModifier.

Resolution requires an Octavia API and Amphora implementation that exposes the required operations with route scope and ordering.

### Required HTTP responses

Gateway API requires HTTP 404 when no attached rule matches.
Forwarding rules with omitted or empty backend references require HTTP 500.
Invalid backends require HTTP 500 for their share of traffic, including when another backend is valid.
The conditions also differ: an omitted backend is not an unresolved reference.
See the [HTTPRoute contract](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/apis/v1/httproute_types.go#L211) and [omitted-backend test](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/conformance/tests/httproute-omitted-backendrefs.go).

The reviewed [Amphora template](https://github.com/openstack/octavia/blob/93689cb7701da65b5aad387bb824c477c64b0f2b/octavia/common/jinja/haproxy/combined_listeners/templates/macros.j2#L151) renders L7 REJECT as `http-request deny`, which returns 403.
It provides backend selection and redirects but no configurable fixed-response action or unmatched-route 404 fallback.
With no matching backend or default backend, [HAProxy returns 503](https://docs.haproxy.org/2.1/configuration.html#4.2-use_backend).
Neither result satisfies the required 404 or 500.

Resolution requires APIs for the required responses and their ordering relative to route selection and weighted backend selection.
Configurable REJECT response codes provide only one primitive.
They do not by themselves establish an unmatched-route fallback, preserve invalid backend shares, or order rejection against forwarding and redirects.
Do not substitute an empty pool, an arbitrary user backend, or a synthetic error server.
Adding such a server or editing Amphora directly would change the project's architecture and ownership contract.

### Redirects without a replacement hostname

When a RequestRedirect filter omits `hostname`, the [Core contract](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/apis/v1/httproute_types.go#L1229) keeps the request hostname.
A hostless or wildcard route can receive hostnames that cannot be enumerated when compiling the route.

Octavia's redirect fields require a fixed absolute HTTP or HTTPS URL, as shown by [URL validation](https://github.com/openstack/octavia/blob/93689cb7701da65b5aad387bb824c477c64b0f2b/octavia/common/validate.py#L51).
The Amphora template emits legacy `redirect location` or `redirect prefix`.
The [HAProxy redirect contract](https://docs.haproxy.org/3.0/configuration.html#4.2-redirect%20prefix) distinguishes these static forms from dynamic `http-request redirect`.
These exposed fields do not provide the general mapping that retains an arbitrary request hostname.

## Phase 4 proof cases

API and model checks precede live proofs.
Each proof depends on the APIs needed for that behavior, not on resolving every blocker above.
Existing Octavia APIs can support independent proofs while other extensions are under review.
The [development sequence](../../ROADMAP.md#develop-and-verify-one-feature-at-a-time) separates integration against pinned development builds from phase completion on released dependencies.
The following primitives inform the proposed mappings in [ADR 0005](adr/0005-core-resource-mapping.md), which defines their verification and acceptance gates.

| Requirement | Octavia constraint |
| --- | --- |
| ClusterIP, headless, and manually managed EndpointSlice backends | Members accept endpoint addresses and ports, but routing and security must already permit Amphora to reach them. |
| Weighted Service backends | A policy selects one pool, whose members have weights from 0 to 256. Service-level distribution needs a separate mapping. |
| Fixed-host redirects | Redirect prefix and status fields exist, but cross-action precedence must be verified. |
| Header and hostname matching | Host and header ACLs and ordered L7 policies exist, subject to API key and value limits. |
| Logical listeners sharing a port | A physical listener can host L7 rules and SNI certificates, but duplicate protocol and port pairs are rejected. |
| HTTPS | Terminated HTTPS accepts certificate references, but safe Barbican ownership is unresolved. |

The [member API](https://docs.openstack.org/api-ref/load-balancer/v2/#create-member) permits addresses outside the supplied subnet when existing routing makes them reachable.
Resolving a ClusterIP or headless Service to EndpointSlices is therefore a candidate, not an established portable mode.
Registering an overlay Pod address does not create connectivity.
Do not modify user Services, OCCM resources, tenant routes, or shared worker security groups to make a test pass.
A managed helper Service would require its own ownership decision and is not an implicit fallback.

The [member types](https://github.com/openstack/octavia/blob/93689cb7701da65b5aad387bb824c477c64b0f2b/octavia/api/v2/types/member.py) expose member weights, while the L7 policy selects one pool.
Assigning equal weights to every endpoint gives a Service with two endpoints twice the share of a Service with one endpoint.
Flattening endpoints into one pool therefore needs a normalization and lifecycle design.
Positive backend ratios are not proven impossible, but the complete invalid-backend semantics remain blocked by the missing HTTP 500 primitive.

Octavia [rejects duplicate listeners using the same underlying protocol and port](https://github.com/openstack/octavia/blob/93689cb7701da65b5aad387bb824c477c64b0f2b/octavia/api/v2/controllers/listener.py#L298).
Core logical listeners distinguished by hostname need a grouping design.
A physical TLS listener may need several SNI certificates even when each logical listener has only one certificate reference.
The additional `GatewayHTTPListenerIsolation` behavior is Extended, as distinguished in the [Gateway API listener contract](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/apis/v1/gateway_types.go#L90).
Do not require that extension as Core or defer the SNI mapping needed for Core configurations.

L7 policy positions alone do not prove ordering between forwarding and redirects.
The Amphora template emits different HAProxy rule types for those actions.
Include a more specific forwarding route overlapping a broader redirect in the compiler proofs.
Also check the [L7 rule key and value limits](https://github.com/openstack/octavia/blob/93689cb7701da65b5aad387bb824c477c64b0f2b/octavia/api/v2/types/l7rule.py#L63) before promising a mapping for every permitted Gateway API input.

Barbican's [Secret metadata API](https://docs.openstack.org/barbican/latest/api/reference/secret_metadata.html) provides a possible identity mechanism.
It does not establish safe ownership by itself.
The certificate ADR must cover project-scoped discovery, duplicate detection, creation before identity metadata is durable, rotation, revocation, and deletion after listener references are removed.
Kubernetes ReferenceGrant authorization and Barbican access control are separate checks.

### Implementation boundary

The current compiler exposes unsupported action entry points in `internal/controller/httproute/httproute_actions.go` for request header modification, redirects, and required HTTP responses.
They return typed validation errors before binding or cloud mutation.
A fixed-host redirect is an unimplemented representable candidate, while arbitrary request-host preservation needs an upstream primitive.
These negative tests preserve the rejection boundary and do not prove the corresponding Core behavior.

The connectivity implementation under [ADR 0004](adr/0004-network-access-and-recovery.md) does not select a future Service forwarding or TLS ownership model.
[ADR 0005](adr/0005-core-resource-mapping.md) records those proposed mappings and the verification needed before acceptance.
The local weight proofs search the bounded member-weight model and show how endpoint health changes its distribution.
They do not establish a general Service-weight mapping or replace traffic measurements.
The [Core inventory](../../test/design/core-mapping.json) tracks the pinned upstream tests separately from implementation evidence.
