# ADR 0005: Core resource mapping and certificate lifecycle

Status: Partially accepted for development

Date: 2026-10-23

## Accepted development scope

On November 17, 2026, the HTTP mapping and binding scope below was accepted for development.
This scope covers typed HTTP child identities, listener grouping, per-Gateway Route bindings and complete inventory, and ordinary forwarding through the existing graph writer.
It permits the corresponding local compiler, controller, adapter, and lifecycle tests without a migration layer.
The controller retains explicit admission limits wherever the missing response and action-ordering APIs prevent safe activation.
This scoped approval does not accept weighted backend mapping, certificate publication, new upstream APIs, or a conformance claim.
Those sections remain proposed and retain their verification gates.

Accepted follow-up work includes listener partial validity, certificate input observation, reusable HTTP and Endpoint E2E scenarios, and a pinned conformance runner.
Keep valid HTTP listeners and their owned resources when a sibling is invalid, and remove only the rejected listener's forwarding resources after the new status is durable.
Observe Secret metadata through indexed watches, authorize certificate references before reading individual payloads, and retain only validation results and source revisions beyond compilation.
This does not enable certificate publication or HTTPS programming.
The E2E additions retain the configured resource budget, while the unmodified conformance suite requires an explicitly dedicated cluster and OpenStack project.
Diagnostic runs may evaluate unadvertised behavior without changing GatewayClass feature declarations or claiming conformance.

## Context

Phase 4 prepares each design for the pinned GATEWAY-HTTP profile before its corresponding Phase 5 implementation.
This proposal records the candidate mappings, their verification cases, and the decisions that still depend on cloud evidence or upstream APIs.
Outside the accepted scope above, it does not authorize a new member mode, resource identity, certificate writer, or public capability claim.
Independently settled parts can be accepted in focused ADRs without accepting every candidate in this proposal.
[ADR 0006](0006-core-model-boundaries.md) accepts the forwarding scope, logical model, per-Gateway concurrency, and direct policy direction.
The concrete environment, remaining physical mappings, API details, and certificate lifecycle remain proposed outside the accepted development scope above.

This proposal separates work in this repository from missing upstream behavior.
Listener grouping, resource identity, parent bindings, input collection, and status handling can be designed and implemented against existing APIs after their scoped decisions are accepted.
An upstream dependency blocks the behavior that needs it, not every part of the Gateway model.
The [upstream work list](../conformance-feasibility.md#upstream-work) identifies those dependencies without assigning speculative API fields or versions.

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

ADR 0006 selects direct endpoint forwarding as the development direction for an explicitly verified network environment.
A ClusterIP is not an endpoint address, and registering a Pod address does not establish connectivity.
[ADR 0007](0007-direct-endpoint-members.md) defines the opt-in IPv4 development mode before a network environment is verified.
A support claim still requires a documented and tested forwarding environment.
Do not infer that capability from Octavia's acceptance of a member address.
Do not change a user Service, worker port, tenant route, or OCCM graph to make the path work.
A helper Service or an alternative forwarding path requires a separate ownership decision if direct endpoints cannot satisfy the selected environment.

The existing NodePort implementation remains governed by ADR 0002 and the [worker eligibility rules](../../reference/class-configuration.md#worker-nodeport-members).
Its port and address-family regression tests do not establish direct endpoint connectivity.

Before accepting an environment profile, demonstrate forward and return traffic for ClusterIP, headless, and manually managed EndpointSlice inputs on the chosen CNI and security configuration.
Verify endpoint addition, removal, terminating endpoints, named ports, and each claimed address family without modifying foreign network resources.

## Listener and route mapping

Build the accepted configuration for all logical listeners and attached routes before assigning physical resources or policy positions.
The complete logical model is accepted under ADR 0006, and the ordinary HTTP mapping below is accepted for the scoped local development above.
The accepted HTTP mapping uses one physical listener per protocol and port, preserving each logical listener's hostname, attachment result, conditions, and `attachedRoutes` count.
HTTP and terminated HTTPS do not become compatible merely because they use the same port.
Keep physical listeners within one Gateway's ownership graph.
Listener changes replace only the affected listener resources, preserving the load balancer, frozen Gateway topology, and unrelated listeners.
Keep route fragments attributable to their exact HTTPRoute UID, including when several logical listeners use a fragment.

Extend the existing provider-neutral Gateway and Route specifications with typed listener, rule, and match collections.
Retain one `ReconcileGatewayGraph` operation and the existing Gateway UID coordinator.
Results identify children by resource key rather than by their position in an input slice.
Gateway topology remains frozen separately from the child listener collection, so adding a listener does not allocate another load balancer.

The proposed resource keys distinguish children within that immutable Gateway and Route identity:

```text
Listener: listener/<protocol>/<port>
Single-backend rule pool: rule/<rule-index>
Policy: listener/<protocol>/<port>/rule/<rule-index>/match/<match-index>/host/<host-index>/path/<variant>
L7 rule: <policy-key>/predicate/<path|host|header-index>
```

For each physical listener and owning Route, rule, and match, collect the effective hostname alternatives from all accepted logical listeners.
Normalize, deduplicate, and sort that union before assigning hostname indexes, so two logical listeners cannot independently claim the same slot.
Assign header indexes after normalization and sorting within the match.
Policy position and the actual match values remain mutable configuration, not ownership identity.
These indexes identify slots within the owning Route, not permanent identities for a hostname or path value.
Reordering rules or changing the sorted alternatives can therefore change the desired contents of an existing slot.
Build the complete desired graph before comparing slots, and do not infer that an unchanged key means unchanged traffic behavior.
Validate each member and monitor against its owned pool and resource key.
Do not share pools across Routes or Gateways.
The single-backend rule proposal does not select a layout for weighted backend groups.

Use one canonical resource-key parser for translation, discovery, deletion, and the ownership audit.
During normal reconciliation, discovery verifies the complete project, cluster, controller, Gateway, and Route identities and the actual parent links.
A recognized key that is absent from the new desired graph can identify an obsolete owned child.
Do not validate obsolete rule indexes against the length of the new spec, because deleting a rule leaves legitimate owned resources to clean up.
Malformed keys, duplicate candidates, foreign parents, or resources whose Route binding is missing stop mutation rather than becoming deletion candidates.
Gateway finalization retains its existing authority to remove recognized descendants whose full Gateway ownership is proven, even if their Route object is absent.
Patch the affected Kubernetes status before destructive replacement, then observe and validate the graph again.
Keep a policy disabled while changing its match or action until its desired rules and target are complete.
Do not promise an atomic traffic switch across several asynchronous Octavia requests.

Evaluate `allowedRoutes`, namespace selectors, parent sections, and listener and route hostname intersections before cloud translation.
Order matches using the pinned Gateway API precedence rules, then creation time and namespace/name tie-breaking where the specification requires them.
Priority belongs to each match within a physical listener, because matches from different Routes can interleave.
The controller calculates that logical order, and the provider assigns contiguous policy positions after PathPrefix expansion.
Preserve match alternatives as separate policies and conditions within a match as conjunctive rules.
Retain the existing complete-path-element handling for PathPrefix.
An invalid listener, rule, or backend must not remove unrelated valid traffic when Core requires partial validity.

Verification must cover equal-port listeners with different hostnames, several parents, overlapping routes, exact header matching, wildcard intersection, ties, and changes in input order.
Verify that logical listeners sharing a physical listener produce distinct hostname slots and deduplicate identical effective matches.
Add and remove listeners and routes while checking both per-listener status and the remaining cloud graph.
Include a specific forwarding match beneath a broader redirect.
For fixed-host redirects, verify scheme and listener-port derivation, 301/302 responses, and preservation of the request path and query.
Octavia policy positions alone do not establish precedence between these different action types.
Accept the forwarding mapping separately from new HTTP actions, with explicit input limits and tests for its supported lifecycle.
The complete Core mapping remains open until action order, required error responses, and API key/value limits are representable.
Check those limits after hostname, path, and header translation, including any PathPrefix suffix or regex escaping.
The feasibility assessment records the provider limits rather than treating a successful local regex compilation as proof of HAProxy behavior.

## Parent bindings and live inventory

Replace singleton Route binding annotations with one typed list owned by this controller, keyed and sorted by Gateway UID.
Each entry records the Gateway namespace, name, and UID, the frozen project, cluster, and controller identities, and Attached or Detaching intent.
The exact HTTPRoute identity still comes from the bound object and remains part of every route-owned cloud resource.
Validate each entry against its matching binding finalizer, and reject incomplete, duplicate, or conflicting entries.
The common Route finalizer remains while any entry exists.
No Secret payload or credentials belong in this list.

Several ParentRefs to one Gateway contribute to one binding while retaining separate parent status entries.
The graph uses the union of their accepted listener attachments, without duplicating the same rule on the same physical listener.
Removing one ParentRef changes only its attachment and status.
Losing the last accepted attachment starts detachment for that Gateway, after the cleanup decision is durable.
Do not recreate a removed parent status entry to record cleanup.

Patch one Gateway's binding from a fresh HTTPRoute revision with optimistic concurrency, preserving every other binding and foreign finalizer.
On conflict, fetch the Route again and recompute that entry instead of retrying a stale whole-list replacement.
Persist the binding and finalizers and return before cloud creation.
Remove an entry and its binding finalizer only after its graph is confirmed absent.
Route deletion processes all recorded Gateways, retaining the bindings that still need cleanup.
Never hold two Gateway coordinator locks at once.

Under the Gateway UID coordinator, read HTTPRoutes across all namespaces and include current ParentRefs to the Gateway and stored bindings for its UID.
The uncached inventory establishes mutation authority, while cached indexes map dependency events to affected consumers.
Complete every list page before planning, and recheck Route revisions and reference permissions before mutation.
Use the same binding parser for the cleanup index and ownership audit.
An incomplete read or malformed binding stops the graph operation without a partial plan.

Keep parent status scoped to the original ParentRef and controller name.
Compute each listener's `attachedRoutes` from distinct attached HTTPRoutes rather than ParentRef or match counts.
A failed Gateway must not erase another Gateway's binding or status, and per-parent observed generations advance only for evaluated outcomes.
Test concurrent binding additions, removal of one of several same-Gateway parents, Gateway recreation with a new UID, Route deletion with one failing Gateway, and restart after each durable checkpoint.

## Backend selection and HTTP actions

Preserve each Service's backendRef traffic share independently of its eligible endpoint count.
ADR 0006 accepts preserving this backend grouping in the desired model without selecting an Octavia pool layout.
The logical separation does not require a particular two-stage scheduling implementation when another mapping preserves the required behavior.
Weight zero contributes no traffic.
Keep invalid backend shares in the routing decision so they receive the required HTTP 500 instead of increasing the valid backend share.
Distinguish an omitted backend list from an unresolved reference when setting status.
Use the upstream response and filter primitives identified in the feasibility assessment rather than an error server or an approximate Octavia action.

Flattening all endpoints into one pool is a candidate, not the selected implementation.
The local exhaustive weight check finds exact uniform-per-Service member weights for small ratios and counterexamples within Octavia's integer range of 0 to 256.
Exact integer representation of every ratio is not required: the pinned [weight contract](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/apis/v1/shared_types.go#L315) allows an implementation precision error for nonzero weights.
Keep a documented precision bound separate from the statistical tolerance of a finite traffic sample.
The exact arithmetic tests do not rule out other mappings or distributions within those bounds.
The health model also shows that independent member health changes can change Service shares before the controller observes a Kubernetes event.

For equal-weight Services with endpoints `A={shared, unique}` and `B={shared}`, uniform endpoint selection within each Service sends three quarters of requests to the shared endpoint.
Deduplicating a union with equal endpoint weights gives one half instead.
This is a counterexample to that simple flattening strategy, not a requirement for uniform endpoint selection within every Service.

First evaluate a normalized single-pool mapping for valid backend groups using the existing member API.
Account for each Service's contribution when endpoint addresses overlap, without creating duplicate members.
Compare it with selection among separate Service pools only if the normalization fails the documented semantics or precision bound.
A new weighted pool API is a candidate solution, not an assumed prerequisite.

Before accepting either mapping, verify weight zero, unequal endpoint counts, rounding, overlapping endpoints, endpoint churn, unhealthy groups, invalid backend shares, and connection reuse.
Keep an invalid reference separate from a valid Service with no ready endpoints.
The [pinned HTTPRoute contract](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/apis/v1/httproute_types.go#L270) requires the invalid share to return 500 and recommends 503 for an empty Service.
This proposal follows that 503 recommendation and preserves the selected Service's share rather than silently moving it to another Service.
Octavia health changes without a Kubernetes event require their own traffic proof, not an assumed equivalence with reference invalidity.
Omitted backends and an all-zero weight set also need explicit response behavior before enabling the corresponding forwarding rule.

## References and certificates

Kubernetes TLS Secrets remain the planned default certificate source.
That source is distinct from the cloud key manager that supplies certificates to Amphora.
The [certificate transport assessment](../conformance-feasibility.md#certificate-transport-and-ownership) explains why the default Secret path still needs a cloud publication mechanism.

Authorize cross-namespace Service and Secret references against ReferenceGrants in the target namespace.
Match the source group, kind, and namespace and the target group, kind, and optional name.
A grant for one target does not authorize another kind or namespace.
Route attachment follows `allowedRoutes` separately.
Authorize a Secret reference before reading its payload, and recheck authorization before publishing a new cloud graph.
Indexes and watches must cover reference changes, grant creation and revocation, Secret rotation, and target deletion.
Revoking a Service grant must stop forwarding to that Service and retain the required HTTP 500 response for its matching traffic.
Deleting its policy and allowing requests to fall through to a broader Route is not a valid revocation implementation.
This makes complete cross-namespace lifecycle support depend on the HTTP response and action-ordering APIs as well as permission checks.
The same dependency applies when an attached Route loses its backend while a broader Route remains valid.
Preserving obsolete forwarding or deleting the specific policy would not implement the required error response.
This blocks a complete Core lifecycle claim, while binding, inventory, status, and ordinary forwarding development can proceed independently.
Do not introduce temporary global traffic suspension or advertise incomplete failure behavior as conformant to bypass the missing response API.

Validate TLS Secret type, required data, certificate parsing, and certificate/private-key agreement before upload.
Keep payloads out of conditions, Events, logs, metrics, and durable bindings.
A logical listener owns its certificate reference and status even when several listeners share a physical TLS listener.
The physical listener needs the default and SNI references required by all accepted logical listeners.

Prefer a single PKCS12 Secret per owned certificate revision over legacy certificate Containers, subject to the ownership proof below.
Initially keep certificate resources within one Gateway's graph rather than sharing them across Gateways.
Within that Gateway, several listeners may reference the same validated Secret revision.
The proposed revision key includes the source Secret UID and a digest of its canonical certificate chain, after validating the matching private key.
A change to unrelated Secret metadata does not require another cloud certificate.
The digest alone is not ownership evidence and is not a public diagnostic label.

Persist a non-secret revision intent in the Gateway binding before creating a certificate resource.
Then publish or rediscover the complete owned revision, update the affected listener references, observe their new configuration, and retire revisions no longer referenced by any owned listener.
Keep the prior revision recorded while a listener still uses it, including across a controller restart.
Cleanup removes listener references before deleting owned certificate Secrets, and retains the Gateway finalizer until every owned revision is confirmed absent.
Grant revocation must stop using the revoked reference even when it prevents uninterrupted traffic.
Never fall back to a previous certificate whose reference is no longer authorized.

Managed certificate ownership remains an unresolved acceptance gate.
Before enabling a certificate writer, select and verify a mechanism that survives a lost create response before metadata or a checkpoint is stored, and supports project-scoped discovery, duplicate rejection, and safe deletion.
Names, creator IDs, ACLs, and cached references alone do not meet this repository's identity requirements.

Prefer to investigate structured ownership in the PKCS12 payload before requiring a Barbican server extension.
The [transport assessment](../conformance-feasibility.md#certificate-transport-and-ownership) records the local format checks and the alternative of atomic initial Secret metadata.
The payload proposal stores the complete authenticated project, cluster, controller, Gateway namespace/name/UID, resource role, source Secret UID, and revision identity in a versioned bag attribute.
The Barbican name narrows the candidate list but never substitutes for validating that payload identity.
Read all pages within the authenticated project and filter exact locator names before reading candidate payloads, because a server-side name search may return partial matches.
Reject duplicate identities or conflicting marker attributes.
Do not use `acl_only` discovery, which can escape the authenticated project's resource scope.

Select a maintained Go codec interface before implementing this representation.
Verify limits on encoded payload and request size, certificate chains, attribute count, and decoding work.
Do not copy the format experiment into production or introduce a general cryptography framework.
Payload-read failures, missing or malformed markers, and ownership mismatches retain the finalizer and stop deletion.
Managed revisions omit automatic Barbican expiration, because certificate validity and discoverable resource lifetime are separate concerns.
The controller removes unused revisions instead of assuming expired certificates disappear from storage safely.

Treat an uncertain create result as a discovery operation before considering another POST.
The full identity makes a returned candidate verifiable, but neither a marker nor a local checkpoint makes POST idempotent.
Keep ambiguous attempts pending rather than repeatedly creating resources, and establish the safe retry boundary in tests for a delayed server response and a crash before sending the request.
Do not claim orphan-free recovery until those cases pass through the actual Barbican backend.

Acceptance also requires malformed and duplicate attributes, two Gateways using the same certificate, reference revocation during rotation, listener references surviving a restart, and deletion with lost payload access.
Verify default and SNI selection for exact and wildcard listener hostnames, including certificates with overlapping SANs, before assuming that a list of certificate references preserves logical listener selection.
The [SNI assessment](../conformance-feasibility.md#certificate-selection-on-a-shared-https-port) identifies the missing hostname association and the conditional Octavia extension.
No payload ownership mechanism or Barbican extension is accepted by this proposal alone.

An optional reference to an operator-managed Barbican certificate is a separate proposed input.
The controller would manage only its owned Listener's reference, never the external certificate's payload, ACL, rotation, or deletion.
Its API must define explicit selection, authorization, project scope, missing-reference behavior, and removal before use.
It is not certificate adoption and cannot replace standard Secret support for Core HTTPS.

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
The implementation sequence follows dependency boundaries:

1. Define typed child keys and collection models, then extend provider observation and diff tests for ordinary forwarding.
2. Implement the Route's per-Gateway binding list, complete live inventory, indexed events, and per-parent status with conflict and restart tests.
3. Connect valid multi-listener and multi-rule forwarding, including obsolete child cleanup and unchanged survivor resources.
4. Add reference revocation and HTTP actions when their required response and ordering primitives are verified.
5. Select the weighted mapping and certificate ownership mechanism independently, then implement their normal and failure lifecycles.
6. Run approved traffic tests and the pinned conformance profile after the corresponding setup and dependencies are ready.

These steps do not add a migration layer, an alternative graph writer, or a new proxy data plane.
Steps 1 and 2 have local implementation and tests.
Step 3 has compiler and adapter coverage for multiple listeners, Routes, and rules, including child cleanup and unchanged survivors.
The controller enables multiple valid HTTP listeners and parent Gateways but retains one selected Route per Gateway and one rule per Route until invalid-backend responses and action ordering are representable.
Listener partial validity and authorized certificate input observation have local implementation and tests.
Rule and backend partial validity, certificate publication, and live traffic evidence remain outstanding.

Acceptance of the remaining proposal requires the following evidence:

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
The remaining proposed mappings do not broaden the implemented member modes.
Scoped acceptance requires the applicable design proofs above and records any unreleased dependencies.
It does not accept unrelated mappings or close Phase 4, which still requires the named upstream releases and outstanding live evidence.
Phase 5 implements accepted mappings incrementally and verifies actual traffic for each feature and its combinations.
Its completion still requires all Core tests and every declared Extended test on the released dependencies.
