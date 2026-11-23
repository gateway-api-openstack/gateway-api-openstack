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

### Certificate selection on a shared HTTPS port

The pinned [Listener TLS contract](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/apis/v1/gateway_types.go#L444) associates certificates with listener hostnames and requires the longest matching SNI.
Octavia accepts a default certificate and a list of SNI certificate references, but no hostname selector for each reference.
The [Amphora driver](https://github.com/openstack/octavia/blob/19.0.0/octavia/amphorae/drivers/haproxy/rest_api_driver.py#L430) writes certificate paths without hostname filters to HAProxy's `crt-list`.
Without those filters, [HAProxy uses certificate CN and SAN names](https://docs.haproxy.org/3.0/configuration.html#5.1-crt-list).

For example, two logical listeners for `a.example.com` and `b.example.com` can select different certificates that both contain those two SANs.
A list of certificate references loses the association between each listener hostname and its selected certificate.
This source analysis identifies a mapping gap, not a measured TLS failure.
Verify overlapping SANs, exact and wildcard hostnames, default selection, rotation, and listener removal before enabling the general Core HTTPS profile.
Assess explicit hostname-to-certificate selection in the Octavia API and Amphora driver if the existing mapping fails those cases.
HAProxy already supports SNI filters, so this does not establish a need for a new HAProxy feature.

## Upstream work

The existing [octavia-lib review 1012714](https://review.opendev.org/c/openstack/octavia-lib/+/1012714) and [Octavia review 1012715](https://review.opendev.org/c/openstack/octavia/+/1012715) implement one capability: selecting the response code of an individual REJECT policy while retaining the default 403.
Both reviews were unmerged when checked on November 17, 2026.
The Octavia change preserves existing policy precedence and unmatched-request handling.
Treat those reviews as one dependency, not as implementations of the remaining behaviors below.

The following work units describe required behavior rather than final API fields or a fixed number of patches.
Several behaviors may share one upstream design.
Required behavior refers to this project's direct Amphora Core target, while conditional extensions need a failing mapping proof before a new API is proposed.

| Work unit | Classification | Proposed scope and acceptance case |
| --- | --- | --- |
| Action ordering | Required. | Select the winning policy independently of whether it forwards, redirects, or returns an error. Verify a specific forwarding rule under a broader redirect and an invalid backend at its original match priority. Preserve existing Octavia behavior through an upstream-agreed compatibility contract. |
| Unmatched-request fallback | Required behavior, new API shape conditional. | Return 404 only after no Route matches. First test whether ordered actions plus the existing REJECT-code proposal can express a final fallback. Add a separate fallback API only if that composition cannot preserve ordinary forwarding and redirects. |
| Request header operations | Required extension. | Add, set, and remove arbitrary request headers for the selected policy. Match the original request before applying transformations, and verify case-insensitive names, multiple values, and isolation from other routes. |
| Redirects using request fields | Required extension. | Preserve the request hostname when no replacement is supplied, and retain unchanged path and query components. Extend the existing redirect behavior, including listener-port derivation, rather than adding a second fixed-host redirect feature. |
| Weighted backend and error selection | Required behavior, mapping and API shape conditional. | Preserve Service weights and the invalid share that must return 500. Evaluate existing member weights first, using a documented precision bound and endpoint lifecycle cases. If they are insufficient, review selection among backend groups and error outcomes together. |
| SNI certificate selection | Mapping gap, extension to verify. | Preserve each logical listener's hostname-to-certificate association when several HTTPS listeners share a port. Verify overlapping SANs and exact and wildcard precedence before proposing explicit SNI selectors. |
| L7 expression limits | Conditional extension. | Establish concrete Gateway API inputs that cannot be represented within current key/value and rule-count limits. Change only the API, storage, and driver limits needed by those proofs, including encoded or escaped length. |

The ordering work follows from Octavia's documented [REJECT, redirect, and forwarding precedence](https://docs.openstack.org/octavia/latest/user/guides/l7.html#policy-position).
The backend selection proof uses the [single redirect pool](https://docs.openstack.org/api-ref/load-balancer/v2/index.html#create-an-l7-policy) and existing [member weights](https://docs.openstack.org/api-ref/load-balancer/v2/index.html#create-member).
For expression limits, compare the [L7 request types](https://github.com/openstack/octavia/blob/19.0.0/octavia/api/v2/types/l7rule.py) and [database columns](https://github.com/openstack/octavia/blob/19.0.0/octavia/db/models.py) with the exact generated representation before proposing a limit increase.

Propose the action-order contract before building separate header, redirect, and response implementations around incompatible execution phases.
Review weighted selection independently, then verify its interaction with error responses.
Under the [Octavia contribution process](https://docs.openstack.org/octavia/latest/contributor/contributing.html#new-feature-planning), discuss larger ordering and backend-selection changes as design proposals, and scope smaller additions with the maintainers.
This is a proposed review sequence, not an upstream-approved plan.
Keep each functional change reviewable with its API documentation and focused tests, with octavia-lib or client support in companion reviews when required.
Library releases and the matching Amphora build are integration dependencies, not additional features.

Existing member registration, ordinary listener and pool creation, hostname and exact header matching, fixed-host redirects, and basic TLS termination do not each need a new Octavia feature.
Their missing controller mapping and lifecycle work stays in this repository, while general SNI selection needs the proof above.

## Phase 4 proof cases

API and model checks precede live proofs.
Each proof depends on the APIs needed for that behavior, not on resolving every blocker above.
Existing Octavia APIs can support independent proofs while other extensions are under review.
The [development sequence](../../ROADMAP.md#develop-and-verify-one-feature-at-a-time) separates integration against pinned development builds from phase completion on released dependencies.
The accepted [model boundaries](adr/0006-core-model-boundaries.md) preserve backend groups and constrain direct endpoint forwarding to an explicitly verified network environment.
The following primitives inform the proposed mappings in [ADR 0005](adr/0005-core-resource-mapping.md), which defines their verification and acceptance gates.

| Requirement | Octavia constraint |
| --- | --- |
| ClusterIP, headless, and manually managed EndpointSlice backends | Members accept endpoint addresses and ports, but routing and security must already permit Amphora to reach them. |
| Weighted Service backends | A policy selects one pool, whose members have weights from 0 to 256. Service-level distribution needs a separate mapping. |
| Fixed-host redirects | Redirect prefix and status fields exist, but cross-action precedence must be verified. |
| Header and hostname matching | Host and header ACLs and ordered L7 policies exist, subject to API key and value limits. |
| Logical listeners sharing a port | A physical listener can host L7 rules and SNI certificates, but duplicate protocol and port pairs are rejected. |
| HTTPS | Kubernetes Secrets can supply certificate data, but Octavia consumes key manager references. Managed publication ownership remains unresolved. |

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

## Certificate transport and ownership

The certificate source and the cloud transport are separate decisions.
The planned default source is a Kubernetes TLS Secret referenced by the Gateway.
Octavia 19.0.0's [Listener request](https://github.com/openstack/octavia/blob/19.0.0/octavia/api/v2/types/listener.py) accepts `default_tls_container_ref` and `sni_container_refs`, not inline certificate and private-key data or Kubernetes Secret references.
The [PKCS12 support contract](https://github.com/openstack/octavia/blob/19.0.0/releasenotes/notes/Support-PKCS12-certificate-objects-1c6e896be9d35977.yaml) permits a single certificate bundle in a key manager Secret instead of deprecated legacy certificate Containers.

The default source would therefore follow this path in a Barbican-backed cloud:

```text
Kubernetes TLS Secret -> validated PKCS12 bundle -> owned Barbican Secret -> Octavia Listener reference -> Amphora
```

Cloud-provider-openstack's [Octavia Ingress controller](https://github.com/kubernetes/cloud-provider-openstack/blob/master/pkg/ingress/controller/controller.go) already implements Secret conversion and Barbican publication.
This establishes a working API path, not this project's ownership guarantee: its name-based discovery and deletion cannot be copied as proof of the full immutable identity required here.

An optional operator-managed certificate reference would use an existing Barbican Secret without creating, rotating, or deleting it.
That is comparable to [AWS LBC Gateway's existing ACM references](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/guide/gateway/gateway/#certificate-discovery-for-secure-listeners), whose documented Gateway path does not synchronize Kubernetes certificate Secrets into ACM.
Reference access and lifecycle ownership are different authorities.
Kubernetes reference authorization and cloud access control must both be defined before adding the optional API.
It does not replace standard Secret-based HTTPS for Core conformance.

Barbican can store and serve the required certificate bundle.
The unresolved issue is safe recovery of controller-created resources.
The [Secret creation API](https://docs.openstack.org/barbican/latest/api/reference/secrets.html) and [user metadata API](https://docs.openstack.org/barbican/latest/api/reference/secret_metadata.html) are separate requests.
If creation succeeds but its response is lost before ownership metadata or the returned ID is persisted, recovery lacks a complete identity with which to authorize mutation or deletion.
A durable intent or an idempotency token alone does not establish that identity.

Two ownership mechanisms remain under review:

| Candidate | Existing support | Remaining proof or implementation |
| --- | --- | --- |
| Structured ownership inside the PKCS12 payload | Standard bag attributes can carry the full identity in the same payload as the certificate. Octavia's existing PKCS12 consumer can extract the certificate and key from such a bundle. | Select a maintained Go codec API for writing and reading the attribute, review payload-read permissions, and verify discovery and recovery through Barbican and Amphora. |
| Initial user metadata in Secret creation | The old approved [Barbican metadata design](https://specs.openstack.org/openstack/barbican-specs/specs/mitaka/add-user-metadata.html#rest-api-impact) includes metadata in POST, but the reviewed current [creation handler](https://github.com/openstack/barbican/blob/master/barbican/api/controllers/secrets.py) and [Secret model](https://github.com/openstack/barbican/blob/master/barbican/model/models.py) do not implement that storage path. | Confirm the design and implementation gap with maintainers, then add transactional initial metadata, validation, tests, and API documentation if this mechanism is selected. |

The payload candidate follows [PKCS12 bag attributes](https://www.rfc-editor.org/rfc/rfc7292.html#section-4.2) and the [serialization API](https://cryptography.io/en/latest/hazmat/primitives/asymmetric/serialization/#pkcs12).
Octavia already supplies a friendly name when [creating PKCS12 data](https://github.com/openstack/octavia/blob/19.0.0/octavia/certificates/manager/barbican.py), while its [consumer](https://github.com/openstack/octavia/blob/19.0.0/octavia/certificates/common/pkcs12.py) extracts the certificate and key without using that name for routing.
Local experiments with synthetic certificates retained a structured ownership attribute and passed certificate/key extraction through the Octavia consumer and Go decoder.
These checks establish file-format compatibility only, not Barbican storage, recovery, access control, or real TLS traffic.
The reviewed [Go PKCS12 library](https://pkg.go.dev/software.sslmate.com/src/go-pkcs12) does not expose the required attribute round trip through its normal certificate-and-key encode/decode API.
Prefer a maintained library enhancement over shipping the experimental ASN.1 manipulation or using deprecated conversion functions.

For this candidate, the Barbican name only narrows discovery.
The controller would validate the complete project, cluster, controller, Gateway, role, and certificate-revision identity from the payload, not infer ownership from the name or a shared certificate fingerprint.
Payload access, bounded decoding, marker integrity, duplicate detection, and project-scoped pagination need separate verification.
The [proposed certificate lifecycle](adr/0005-core-resource-mapping.md#references-and-certificates) defines the recovery cases before enabling either mechanism.

Barbican server changes are therefore conditional, not a prerequisite established by the current evidence.
If initial user metadata is selected, the smallest useful server review covers atomic Secret and metadata persistence, rollback, permission and quota validation, and compatibility with requests that omit metadata.
Test creation-response loss followed by project-scoped rediscovery and exact identity checks.
A client-library change can follow separately.
Metadata filtering is a possible discovery optimization, and general idempotency keys or client-assigned UUIDs are separate proposals, not requirements to bundle into this first review.
Initial metadata makes ownership discoverable but does not by itself make repeated POST requests idempotent.

Rotation must also account for [immutable Secret payloads](https://docs.openstack.org/api-guide/key-manager/secrets.html#single-step-secret-creation) by creating and switching to a new revision before retiring an unreferenced owned revision.

Octavia supports operator-configured [certificate managers](https://docs.openstack.org/octavia/latest/configuration/configref.html#certificates), including a Castellan integration.
A cloud with a suitable alternative backend can avoid Barbican, but this is cloud operator configuration, not a controller option that sends Kubernetes Secrets directly to stock Amphora.
Replacing the key manager or adding an inline certificate transport is outside the accepted design and requires its own API, security, and recovery review.

## Implementation boundary

The current compiler exposes unsupported action entry points in `internal/controller/httproute/httproute_actions.go` for request header modification, redirects, and required HTTP responses.
They return typed validation errors before binding or cloud mutation.
A fixed-host redirect is an unimplemented representable candidate, while arbitrary request-host preservation needs an upstream primitive.
These negative tests preserve the rejection boundary and do not prove the corresponding Core behavior.

The connectivity implementation under [ADR 0004](adr/0004-network-access-and-recovery.md) does not select a future Service forwarding or TLS ownership model.
[ADR 0005](adr/0005-core-resource-mapping.md) records those proposed mappings and the verification needed before acceptance.
The local weight proofs search the bounded member-weight model and show how endpoint health changes its distribution.
They do not establish a general Service-weight mapping or replace traffic measurements.
The [Core inventory](../../test/design/core-mapping.json) tracks the pinned upstream tests separately from implementation evidence.
