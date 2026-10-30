# ADR 0005: Core resource mapping and certificate lifecycle

Status: Proposed

Date: 2026-10-23

## Context

Phase 4 prepares each design for the pinned GATEWAY-HTTP profile before its corresponding Phase 5 implementation.
This proposal records the candidate mappings, their verification cases, and the decisions that still depend on cloud evidence or upstream APIs.
It does not authorize a new member mode, resource identity, certificate writer, or public capability claim.
Independently settled parts can be accepted in focused ADRs without accepting every candidate in this proposal.

The [feasibility assessment](../conformance-feasibility.md) is the source record for Octavia limitations.
The [Core inventory](../../../test/design/core-mapping.json) assigns every pinned upstream Core test to a requirement group.
CI compares that inventory with the pinned upstream source without running the conformance suite.
An inventory match is not a passing conformance result or proof that every API requirement is covered by those tests.

## Service forwarding

Resolve a backend reference to its authorized Service, selected Service port, and EndpointSlices.
Use the Service namespace and `kubernetes.io/service-name` label to find slices, including manually managed slices without a Service owner reference.
Match EndpointSlice port names to Service port names rather than to named `targetPort` values.
Each slice supplies its own resolved port, which can differ between Pods for the same named target port.
Apply endpoint readiness, termination, and address family independently of NodePort worker eligibility.

The candidate for ordinary ClusterIP and headless Services is forwarding to resolved endpoint addresses through existing routing.
A ClusterIP is not an endpoint address, and registering a Pod address does not establish connectivity.
The class capability must describe the verified address families and forwarding environment before this member mode can be enabled.
Do not infer that capability from Octavia's acceptance of a member address.
Do not change a user Service, worker port, tenant route, or OCCM graph to make the path work.
A helper Service or an alternative forwarding path requires a separate ownership decision if direct endpoints cannot satisfy the selected environment.

The existing NodePort implementation remains governed by ADR 0002 and the [worker eligibility rules](../../reference/class-configuration.md#worker-nodeport-members).
Its port and address-family regression tests do not enable direct endpoint members.

Before accepting the forwarding design, demonstrate forward and return traffic for ClusterIP, headless, and manually managed EndpointSlice inputs on the chosen CNI and security configuration.
Verify endpoint addition, removal, terminating endpoints, named ports, and each claimed address family without modifying foreign network resources.

## Listener and route mapping

Build the accepted configuration for all logical listeners and attached routes before assigning physical resources or policy positions.
Group compatible listeners by port and data plane protocol, preserving each logical listener's hostname, attachment result, conditions, and `attachedRoutes` count.
HTTP and terminated HTTPS do not become compatible merely because they use the same port.
Keep physical listeners within one Gateway's ownership graph.
Keep route fragments attributable to their exact HTTPRoute UID, including when several logical listeners use a fragment.
Finalize the additional listener, rule, and match identity keys before implementing shared physical resources.

Evaluate `allowedRoutes`, namespace selectors, parent sections, and listener and route hostname intersections before cloud translation.
Order matches using the pinned Gateway API precedence rules, then creation time and namespace/name tie-breaking where the specification requires them.
Preserve match alternatives as separate policies and conditions within a match as conjunctive rules.
Retain the existing complete-path-element handling for PathPrefix.
An invalid listener, rule, or backend must not remove unrelated valid traffic when Core requires partial validity.

Verification must cover equal-port listeners with different hostnames, several parents, overlapping routes, exact header matching, wildcard intersection, ties, and changes in input order.
Add and remove listeners and routes while checking both per-listener status and the remaining cloud graph.
Include a specific forwarding match beneath a broader redirect.
For fixed-host redirects, verify scheme and listener-port derivation, 301/302 responses, and preservation of the request path and query.
Octavia policy positions alone do not establish precedence between these different action types.
This mapping remains open until the full action order and API key/value limits are representable.

## Backend selection and HTTP actions

Select a Service according to its backendRef weight before selecting one of that Service's eligible endpoints.
Weight zero contributes no traffic.
Keep invalid backend shares in the routing decision so they receive the required HTTP 500 instead of increasing the valid backend share.
Distinguish an omitted backend list from an unresolved reference when setting status.
Use the upstream response and filter primitives identified in the feasibility assessment rather than an error server or an approximate Octavia action.

Flattening all endpoints into one pool is a candidate, not the selected implementation.
The local exhaustive weight check finds exact uniform-per-Service member weights for small ratios and counterexamples within Octavia's integer range of 0 to 256.
It does not rule out other mappings or distributions within the conformance test's tolerance.
The health model also shows that independent member health changes can change Service shares before the controller observes a Kubernetes event.

For equal-weight Services with endpoints `A={shared, unique}` and `B={shared}`, the shared endpoint receives three quarters of requests under two-stage selection.
Deduplicating a union with equal endpoint weights gives one half instead.
This requires a contribution model for overlapping endpoints, not duplicate member creation.

Before accepting a pool mapping, verify weight zero, unequal endpoint counts, rounding, overlapping endpoints, endpoint churn, unhealthy groups, invalid backend shares, and connection reuse.
Record the traffic distribution tolerance and the behavior when a selected Service has no healthy endpoints.
Do not declare a general mapping from the small arithmetic witnesses alone.

## References and certificates

Authorize cross-namespace Service and Secret references against ReferenceGrants in the target namespace.
Match the source group, kind, and namespace and the target group, kind, and optional name.
A grant for one target does not authorize another kind or namespace.
Route attachment follows `allowedRoutes` separately.
Authorize a Secret reference before reading its payload, and recheck authorization before publishing a new cloud graph.
Indexes and watches must cover reference changes, grant creation and revocation, Secret rotation, and target deletion.

Validate TLS Secret type, required data, certificate parsing, and certificate/private-key agreement before upload.
Keep payloads out of conditions, Events, logs, metrics, and durable bindings.
A logical listener owns its certificate reference and status even when several listeners share a physical TLS listener.
The physical listener needs the default and SNI references required by all accepted logical listeners.

The proposed rotation order is to validate and authorize the replacement, persist a non-secret revision checkpoint, create and discover owned certificate resources, switch listener references, observe the new configuration, then retire unreferenced old resources.
Cleanup reverses references before deleting containers and secrets.
Grant revocation must stop using the revoked reference even when it prevents uninterrupted traffic.
Never fall back to a previous certificate whose reference is no longer authorized.

Barbican ownership remains an unresolved acceptance gate.
The [Secret API](https://docs.openstack.org/barbican/latest/api/reference/secrets.html) and [metadata API](https://docs.openstack.org/barbican/latest/api/reference/secret_metadata.html) create the secret and its user metadata in separate transactions.
A crash or lost response between them can leave a resource that cannot be rediscovered by complete ownership identity.
The [container API](https://docs.openstack.org/barbican/latest/api/reference/containers.html) has a separate lifecycle and does not inherit an ownership contract from child secret metadata.
Names, creator IDs, ACLs, and cached references alone do not meet this repository's identity requirements.
Before enabling a certificate writer, select and verify a mechanism that survives each creation boundary and supports project-scoped discovery, duplicate rejection, and safe deletion.

## Capability and implementation gates

Keep API availability, environmental connectivity, and conformance evidence separate.
The current Octavia and Neutron capability checks continue to govern the implemented network modes under ADR 0004.
Future Core actions need explicit checks for the required APIs.
Support claims and phase completion additionally require named released Octavia versions.
Development integration may use pinned upstream patches under the [roadmap sequence](../../../ROADMAP.md#develop-and-verify-one-feature-at-a-time) once the corresponding scoped design is accepted.
That design must identify the provisional API contract, capability checks, and tests to revisit when the upstream API changes.
Do not assign speculative microversions or enable features from a provider name alone.
Certificate support additionally requires the accepted Barbican ownership mechanism and the corresponding project permissions.
Unknown capability blocks new programming but does not authorize deleting an existing graph or prevent validated cleanup.

`supportedFeatures` remains unchanged until the corresponding behavior passes conformance.
Acceptance of this proposal requires the following evidence:

| Requirement group | Verification before acceptance |
| --- | --- |
| Gateway status | Reviewed mapping of accepted, rejected, pending, partial, and cleanup outcomes to the pinned conditions and observed generations. |
| Service forwarding | Port and family resolution tests, plus the approved endpoint reachability experiments above. |
| Listeners and routing | Deterministic compiler cases and API proofs for grouping, attachment, ordering, partial validity, and cleanup. |
| Backend selection and errors | A selected mapping with API and traffic proofs for endpoint counts, health changes, invalid shares, and the required HTTP response actions. |
| HTTP filters | API and traffic proofs for request-header and redirect operations with the required scope and ordering. |
| Backend ReferenceGrants | Authorization and revocation tests that preserve unrelated routes and reject stale permissions before mutation. |
| Certificates | Secret validation, SNI, revocation, and rotation cases, plus an ownership mechanism verified across lost responses and process restarts. |

## Consequences

Phase 4 local tests can expose incorrect candidates and protect current behavior without adding an unused parallel controller.
The proposed mappings remain separate from the implemented NodePort contract.
Scoped acceptance requires the applicable design proofs above and records any unreleased dependencies.
It does not accept unrelated mappings or close Phase 4, which still requires the named upstream releases and outstanding live evidence.
Phase 5 implements accepted mappings incrementally and verifies actual traffic for each feature and its combinations.
Its completion still requires all Core tests and every declared Extended test on the released dependencies.
