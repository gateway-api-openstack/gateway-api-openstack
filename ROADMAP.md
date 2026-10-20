# Roadmap

This roadmap sets the order of work and completion criteria for gateway-api-openstack.
It is a planning document, not a release promise.
The target is an OpenStack implementation listed upstream as Conformant for GATEWAY-HTTP.
Phase 5 requires the pinned profile's complete Core suite and every declared Extended feature to pass.
Partial results document progress but do not satisfy that milestone.

The Phase 3 implementation is complete, with broader validation and release work remaining.
The project remains pre-alpha.
The [compatibility record](docs/providers/compatibility.md) describes the evidence available for the current implementation.

Each controller release pins its Gateway API version.
The current baseline is Gateway API v1.6.1 Standard Channel.

## Product contract

The controller provisions Octavia Amphora resources directly for HTTP and planned terminated HTTPS traffic.
One Gateway owns one load balancer and its dependent graph.
One deployment uses one OpenStack cloud, region, authenticated project, and credential scope.
The controller does not adopt existing resources, modify OCCM graphs, or manage tenant network topology.
Unsupported behavior is rejected through standard Gateway API status.
The [architecture](docs/design/architecture.md) defines the ownership and reconciliation rules that every phase must preserve.

## Phase overview

| Phase | Status | Outcome | Exit gate |
| --- | --- | --- | --- |
| 0: Feasibility | Historical | Establish Amphora primitives and ownership boundaries | Retain the probe and resolve design decisions in the phases that need them |
| 1: HTTP slice | Implemented and development-tested | One Gateway, listener, route, and NodePort backend | Published evidence is required for release compatibility claims |
| 2: Reconciliation | Development milestone complete | Reliability foundations for the HTTP and NodePort path | Baseline OpenStack E2E passed, with remaining work carried into Phase 3 |
| 3: Class API | Implemented, with validation remaining | Typed topology configuration, one graph writer, and worker members | Broader live coverage and artifacts for outside testing |
| 4: Connectivity and Core design | Planned, with API blockers | Safe connectivity and validated Core mappings | Accepted designs, resolved Core API blockers, and scoped OpenStack evidence |
| 5: HTTP/HTTPS conformance | Planned, depends on Phase 4 | Complete GATEWAY-HTTP Core implementation | All Core and declared Extended tests pass, with safe resource lifecycle |
| 6: Production candidate | Later | Repeatable operations and community evidence | Two independent clouds, hardened releases, and an eligible successful conformance report |
| 7: Stable | Later | Supported, upgradeable Amphora contract | Stable API, adopter upgrades, upstream Conformant listing, and release evidence |

Version milestones below are planning targets, not published releases.
The [development priorities](docs/development-priorities.md) identify the next actions.
Community maturity, independent review, and maintainers from more than one organization are separate release requirements.
A Kubernetes SIG or OpenInfra home is a later community decision, not an engineering milestone.

## Evidence and compatibility rules

Compatibility applies to a particular release and environment.
Use the [compatibility record](docs/providers/compatibility.md) for environment requirements and the [test evidence policy](docs/reports/README.md) for publication rules.
Keep exact build details, raw logs, and private environment configuration local during development.

`Probed` means an API primitive worked in one environment.
`Verified` means a controller revision passed named tests in a recorded environment.
`Supported` means a release and environment are covered by a maintained compatibility and support policy.
`Conformant` means the pinned upstream suite passed for the claimed profile and declared features.

GatewayClass `supportedFeatures` is a runtime declaration consumed by the suite.
Declare a feature only after its complete required behavior and combinations pass the pinned tests.
Gateway and HTTPRoute declarations cover each resource's full Core behavior, not a controller-specific subset.

Every phase must preserve these invariants:

- Repeated reconciliation converges without further OpenStack mutations or semantic status changes.
- Deletion resumes after a restart and cannot remove foreign resources.
- Unsupported input is reported before cloud mutation.
- Missing evidence remains explicitly unverified.
- Public APIs follow their declared compatibility policy.

## Phase 0 - Feasibility and ownership discovery

**Status:** Historical baseline.

**Milestone:** `phase-0-initialize`

The capability probe established the initial Octavia and Neutron primitives and the separate ownership boundary with cloud-provider-openstack.
It remains useful for environment discovery but does not replace controller E2E.
Unresolved design decisions remain gates for the phases that depend on them.

## Phase 1 - Constrained HTTP implementation

**Status:** Implementation complete, with formal release evidence pending.

**Milestone:** `v0.1.0`

Phase 1 introduced the HTTP and NodePort path described in the [current implementation](README.md#current-implementation).
It established exact controller selection, immutable cloud identity, finalizers, standard status, and the Gophercloud adapter boundary.
Development E2E covered traffic, status, restart, converged no-op behavior, ordered deletion, and cleanup.
Formal compatibility claims still require reviewed evidence for the release and environment.

## Phase 2 - Safe and efficient reconciliation

**Status:** Development milestone complete.

**Milestone:** `v0.2.0`

Phase 2 established the reliability foundations for the Phase 1 path.
It added classified provider outcomes, bounded asynchronous progress, drift observation, resumable finalization, indexed dependency watches, and the read-only ownership audit.
The baseline OpenStack E2E passed.
The complete graph writer moved into Phase 3 under [ADR 0001](docs/design/adr/0001-gateway-graph-writer.md).
The [architecture](docs/design/architecture.md#reconciliation-model) describes the resulting control loop.

### Work carried into Phase 3

| Work | Status and remaining gate |
| --- | --- |
| Complete graph writer and durable route lifecycle | Implemented under ADR 0001, with controller, adapter, race, and envtest coverage |
| Class configuration and worker selection | Implemented under ADR 0002, with schema and API server tests |
| Event handling and model efficiency | Local read budgets, burst tests, and benchmarks are available. Measure cloud convergence before increasing concurrency |
| Backend transitions and class isolation | Development E2E covers selected scenarios. Broader topology isolation and traffic continuity need evidence |
| External deletion, quota, rate limiting, timeouts, and Octavia failures | Reproducible fault procedures and live results remain outstanding |
| Blocked finalization and operator recovery | Validate the recovery workflow and ownership audit in an approved test environment |
| Conformance gaps | Known Core gaps and Octavia API blockers are documented. Full-profile validation depends on Phases 4 and 5 |
| Release artifacts and scheduled E2E | Publish an immutable image and configure a dedicated test environment with leak checks |
| Project readiness for outside testing | Review governance, support, security contacts, and release responsibilities |

Use the [compatibility record](docs/providers/compatibility.md#development-validation) for current live results and the [conformance guide](docs/testing-conformance.md) for the supported test scope.
These development checks do not replace release evidence.

## Phase 3 - Class configuration and topology API

**Status:** Implementation complete, with broader validation and release work remaining.

**Milestone:** `v0.3.0`

Phase 3 introduces OpenStackGatewayClassConfig and freezes resolved settings on each Gateway before cloud creation.
It adds eligible worker selection and the shared graph writer.
[ADR 0002](docs/design/adr/0002-class-configuration.md) defines the class API, frozen bindings, and worker rules.
[ADR 0003](docs/design/adr/0003-project-organization.md) defines the organization and API domain.
The [class configuration reference](docs/reference/class-configuration.md) describes the schema and operator workflow.

Selectors resolve to exactly one permitted resource.
Read-only capability checks validate the selected Amphora API and topology, while live tests establish permissions, quota, and backend connectivity.
The Go types, DeepCopy methods, and CRD are maintained together and checked by schema tests and envtest.
This development API provides no Phase 2 migration or rollback promise.

### Phase 3 validation and release gates

Local tests cover frozen bindings, separate class topologies, worker eligibility, ownership conflicts, event bursts, and one mutation per graph pass.
Release archives include the matching CRD, RBAC, Kustomize resources, and examples.
The remaining work is:

- Extend live coverage across distinct class topologies and validate backend traffic continuity.
- Complete the fault and recovery evidence listed above.
- Keep the Core gap analysis and diagnostic scope current as implementation changes.
- Measure rate limiter wait, Octavia pending duration, and convergence before increasing concurrency.
- Publish an immutable pre-alpha image for evaluation.
- Configure scheduled E2E and leak checks in an approved dedicated environment.

## Phase 4 - Core design, network, address, and security automation

**Status:** Planned, with Core API blockers identified in Octavia 19.0.0.

**Milestone:** `v0.4.0`

Phase 4 validates the designs needed for full GATEWAY-HTTP Core support and reduces manual network configuration without taking ownership of the cluster's network topology.
The initial target is Gateway API v1.6.1 Standard Channel.
Reassess the requirements when changing the pinned Gateway API version.

### Core feasibility before implementation

The [feasibility assessment](docs/design/conformance-feasibility.md) records the Octavia API blockers and proof cases.
The reviewed API lacks arbitrary request header modification, the required HTTP 404 and 500 responses, and redirects that retain an arbitrary request hostname.
Resolve these gaps through reviewed Octavia API and Amphora changes, then record the released version and capability checks required by the controller.
Independent connectivity work can proceed while those blockers remain, but Phase 4 cannot close.

Before implementing the corresponding Phase 5 features, accept designs for:

- Service forwarding that accepts the required Service inputs without modifying user Services or OCCM graphs
- logical listener grouping, route precedence, partial validity, status, and cleanup
- request filters, error responses, redirects, and Service-level backend weights
- TLS Secret validation, ReferenceGrant revocation, SNI, and Barbican resource lifecycle

Validate the [proof cases](docs/design/conformance-feasibility.md#phase-4-proof-cases) through API review and model tests before live testing.
Member mode, resource mapping, capability requirements, and certificate ownership changes require accepted ADRs.
A proxy, synthetic error backend, or direct Amphora configuration change would require a separate product decision.
Live proofs require an approved resource scope and cleanup evidence.

### Floating IP lifecycle

Phase 3 implements class modes `None` and `Allocate`, external network resolution, and cleanup of a proven detached Floating IP after its load balancer disappears.
It also detects detachment while the load balancer exists and stops provisioning to prevent duplicate allocation.
Phase 4 completes the remaining connectivity and recovery behavior:

- Recover an owned Floating IP that becomes detached while its load balancer still exists.
- Validate external deletion, quota failure, and address recovery with live traffic tests.
- Report failures in the VIP or Floating IP network path with useful diagnostics.
- Preserve the rule that existing addresses are never adopted and foreign addresses on a VIP port cause an ownership conflict.

### Frontend security groups

`OctaviaManaged` remains the default: the controller omits `vip_sg_ids` and Octavia manages listener access.
A `Referenced` mode may pass security groups supplied by the user through `vip_sg_ids` only after the controller verifies API support.
The controller validates the groups and does not change their rules.

[Custom VIP security groups](https://docs.openstack.org/octavia/latest/contributor/specs/version15.0/custom-security-groups-for-VIP-ports.html) were added in Octavia API microversion 2.29 with Octavia 16.0 (OpenStack 2025.1).
They are applied to the VIP and Amphora VRRP ports.
Octavia retains its internal VRRP and HAProxy peer rules but no longer manages listener ingress rules.
The option is incompatible with listener `allowed_cidrs` and SR-IOV load balancers.
Those consequences must be validated and visible before a Gateway is programmed.
The currently pinned Gophercloud version does not expose `vip_sg_ids` in its typed create options, so implementation also requires an appropriate dependency update or a narrow, tested extension.
The initial implementation will rely on frontend security groups managed by Octavia and will not create a separate frontend group.

### Backend security groups

Start with `Referenced` and `Unmanaged` modes.
`Referenced` validates the configuration supplied by the operator without modifying the referenced group.
`Unmanaged` leaves backend security to the environment.
API discovery can validate structural prerequisites, but it cannot prove that Amphora members can reach NodePort backends.
Octavia health status, an active reachability check, or end-to-end test results from OpenStack must demonstrate that connectivity.

A `Managed` mode may be introduced only after its ADR defines a narrow shared ownership contract.
Worker Neutron ports belong to CAPO, another controller, or the operator.
Changing their `security_groups` list is a mutation of a foreign object even when this controller created the group being attached.

If accepted, managed mode follows these rules:

- Enable it only through an explicit class opt-in.
- Create only groups and rules tagged with the controller's complete ownership identity.
- Map each Node provider ID to its Nova server UUID and selected address.
  Then validate exactly one Neutron port in the authenticated project by `device_owner`, `device_id`, selected fixed IP and subnet, and `port_security_enabled`.
- Preserve every foreign security group association.
  Add or remove only the group ID added by this controller.
- Include the observed Neutron revision in each update where the API supports it.
  If the port changes concurrently, reject the mutation and observe the port again before retrying.
- Reconcile only NodePorts still referenced by managed routes.
- Use exact ports by default.
  Opening the whole NodePort range requires an explicit choice between quota use and network exposure.
- Take backend source CIDRs from explicit configuration or verified environment evidence.
  Do not assume the VIP subnet is the source.
- Define the identity and reference counting for a shared group, along with restart recovery, uninstall, and rollback behavior.

Both `externalTrafficPolicy: Cluster` and `externalTrafficPolicy: Local` need separate tests because their member selection differs.
Node addition, removal, address change, readiness, and EndpointSlice changes must converge the member set and security access together without causing a burst of OpenStack API calls.

### Diagnostics

Add a read-only diagnostic command or dedicated binary for authentication, Amphora and microversion checks, network resolution, mapping from each Node to a Neutron port, NodePort reachability prerequisites, permissions to read and update security groups, Floating IP quota, and optional Barbican access.
Diagnostic output must be redacted and suitable for attaching to an issue.

Compatibility work in this phase records IPv4, IPv6, or dual-stack behavior instead of assuming the IP family.
It also records Amphora topology and the tested kube-proxy implementation or replacement.
Unsupported combinations are reported as such before they become a release promise.

### Phase 4 exit criteria

- Every required Core behavior has a reviewed mapping and a corresponding verification case.
  No unresolved public API blocker is treated as supported or deferred out of Core.
- ADRs cover the selected Service forwarding path, listener and route mapping, backend behavior, capability checks, and certificate lifecycle before their implementation.
- Any required Octavia extensions are available in a named release and have focused evidence for the selected Amphora environment.
  A proposal, development branch, or capability probe alone does not close this gate.
- Network selection never chooses arbitrarily and does not mutate OpenStack when resolution is ambiguous.
- The controller safely recovers from Floating IP detachment and from drift in security rules that it created.
- Frontend `OctaviaManaged` and `Referenced` modes and backend `Referenced` and `Unmanaged` modes have documented results from an OpenStack environment.
- If `Managed` backend security is enabled, Node lifecycle and concurrent port changes pass E2E without changing or removing foreign groups or rules.
- The ADR for modifying worker ports owned by another component decides whether managed backend security will be supported.
  If managed mode is accepted, the tests must show that NodePort backends are reachable without manually changing the rules on each port.
  Otherwise, the operator remains responsible for the referenced configuration.
- No network, subnet, router, CAPO resource, or OCCM resource is adopted or deleted.
- The recurring environment job exercises both claimed NodePort traffic policies, endpoint changes, and leak detection without an unbounded burst of Kubernetes or OpenStack API calls.

## Phase 5 - Complete GATEWAY-HTTP conformance

**Status:** Planned, dependent on the Phase 4 designs and resolution of Core API blockers.

**Milestone:** `v0.5.0`

Phase 5 implements every Core requirement of the pinned GATEWAY-HTTP profile after the corresponding Phase 4 design is validated.
The profile includes Gateway, HTTPRoute, ReferenceGrant, and HTTPS termination.
Completion requires all Core tests and all tests for declared Extended features to pass.
Known Core failures, skipped required tests, and setup blockers prevent completion.

### Listener and route compiler

Support multiple Gateway listeners, attached HTTPRoutes, parent Gateways, rules, matches, and route hostnames.
Enforce `allowedRoutes`, namespace selectors, `parentRef.sectionName`, listener and route hostname intersections, and accurate `attachedRoutes` status.
Handle listener addition and removal and update all evaluated conditions with the correct observed generation.
Keep valid listeners and rules working in the invalid-reference and unsupported-protocol cases required by Core.
Follow the specification's separate conflict rules instead of applying one blanket rejection policy.

The compiler owns the entire listener L7 graph.
Gateway API matches in a rule are alternatives, while conditions inside a match are conjunctive.
Octavia L7 policies are ordered alternatives whose rules are conjunctive.
All attached routes therefore contribute to one deterministic policy order.
Precedence, tie-breaking, and Octavia policy positions must be stable across restarts and map iteration.

Gateway API PathPrefix matches complete path elements, while Octavia `STARTS_WITH` is a raw string prefix.
`/foo` continues to compile into an exact `/foo` policy and a `/foo/` prefix policy so it does not match `/foobar`.

When valid Gateway listeners share a port, use the accepted mapping to preserve their protocol, hostname, certificate, attachment, and status semantics.
Octavia does not allow a separate physical listener for every logical listener on the same port.
An unresolved mapping for a required combination blocks completion.
The additional `GatewayHTTPListenerIsolation` behavior is Extended and becomes mandatory only if declared.

### Supported HTTP behavior

Implement exact and wildcard hostnames, Exact and PathPrefix paths, default routes, and exact header matches with the required precedence.
Implement route-level RequestHeaderModifier add, set, and remove operations and Core RequestRedirect hostname and 301/302 behavior.
Redirects must preserve the fields the filter does not replace.
Request header modification and redirects that retain an arbitrary request hostname depend on resolving the Octavia API blockers identified in Phase 4.

Return HTTP 404 when no attached rule matches.
Return HTTP 500 for forwarding rules with omitted or empty backend references and for invalid backends as required by the specification, with the corresponding Accepted and ResolvedRefs conditions.
A rejected or empty Octavia pool is not a substitute for the required response.
These responses also depend on resolving the Phase 4 API blocker.

Implement multiple Service backends, proportional weights, weight zero, and the required distribution of failures when only part of the backend configuration is invalid.
Preserve Service-level behavior as endpoint counts and health change.
The accepted design and pinned tests must establish this behavior rather than infer it from Octavia member weights.

Method and query matching, response header modification, backendRef-level request header modification, URLRewrite, mirroring, and route timeouts or retries are outside the initial Extended claim.
Optional requested addresses, listener isolation, or other Extended features require an explicit claim and every corresponding test to pass before publication.
Project-specific policies and arbitrary ExtensionRef support do not replace Core behavior.

### Required Service backend inputs

Accept ordinary ClusterIP Services, headless Services, and Services backed by manually managed EndpointSlices through the forwarding design selected in Phase 4.
Support the required endpoint and port resolution without requiring users to change their Service type.
Do not modify user Services, take over OCCM resources, or create tenant network routes implicitly.
Direct endpoint members are a candidate only where existing Amphora and endpoint routing has been verified.
NodePort forwarding may remain available, but a NodePort-only user contract cannot satisfy this phase.

### Cross-namespace references

Add ReferenceGrant handling for Service and Secret references.
Removal of a grant must revoke the reference and safely converge the dependent graph.
Include `ReferenceGrant` in `supportedFeatures` only after every applicable combination with the controller's other reported features passes conformance.
Attachment of a route in another namespace to a Gateway follows Gateway API's `allowedRoutes` rules and is not treated as a generic object reference grant.

### Terminated HTTPS

Support terminated HTTPS from Kubernetes TLS Secrets through Barbican resources created by the controller.
Support the required certificate reference for each logical listener.
When logical listeners share a physical listener, include the SNI certificates needed to preserve those Core configurations.
Do not defer that mapping as optional multiple-certificate support.

The TLS design must cover Secret validation, redaction of private keys and certificate data, cross-namespace authorization, immutable identity for Barbican resources, certificate rotation without downtime, crash recovery between old and new certificates, and deletion of only the secrets and containers created by the controller.
If Barbican cannot expose enough identity to prove ownership, the controller does not manage that resource type until a safe identity mechanism has been designed and tested.

The conformance environment must provide the certificate service required by the accepted HTTPS design.
An HTTP-only development environment can still run selected diagnostics, but it cannot establish GATEWAY-HTTP completion.

### Addresses and backend policy

Support representable `Gateway.spec.addresses` requests as Octavia VIP addresses: dynamic allocation or a requested IP inside the validated VIP subnet.
Reject an unsupported address type, an address outside the binding, or an address already in use.
The class Floating IP policy remains a separate choice for the external address.
The API contract must state whether `Allocate` adds a new FIP to a requested VIP or rejects that combination before mutation.

Introduce a namespaced OpenStackBackendPolicy only when documented use cases require health monitor settings for individual Services.
If added, the first version will be an experimental `v1alpha1` Direct Policy Attachment.
It will use a `targetRef` to a Service in the same namespace, or to a named Service port through `sectionName`, and contain only health monitor settings.
The CRD carries the `gateway.networking.k8s.io/policy: Direct` label and reports standard `Accepted`, `Conflicted`, and `TargetNotFound` reasons through `PolicyAncestorStatus` entries for each Gateway and controller.
Session persistence, pool algorithms, and connection limits do not belong in the first version without separate evidence.

No particular internal member mode is required by Gateway API.
The mode selected in Phase 4 must accept the required Service inputs and pass the complete Core behavior in the supported environment.
If it uses endpoint IPs, verify routing, EndpointSlice ready, serving, and terminating conditions, draining, CNI compatibility, security, and source IP behavior.
Any additional member mode needs its own compatibility evidence.

### Phase 5 exit criteria

- The pinned GATEWAY-HTTP profile passes all Core tests and all tests for declared Extended features using the supported controller configuration.
  No required test is skipped, and fixture changes do not remove or weaken behavior under test.
- The full upstream setup works with the required Service inputs and namespace relationships.
- Multiple HTTP and terminated HTTPS listeners and routes share one Gateway without ownership conflicts or unstable policy order.
- Route precedence and PathPrefix matching by complete path elements pass unit tests, conformance tests, and tests in OpenStack.
- Cross-namespace Service and Secret access is granted and revoked exactly as Gateway API specifies.
- Certificate creation, rotation, restart recovery, and deletion leave no orphaned Barbican resources.
- Every reported `supportedFeatures` entry has a matching pinned conformance result.
- The suite produces an unmodified successful profile report with reproducible instructions and an immutable implementation version.
- Any unresolved Core API limitation keeps this phase incomplete.
  Publish the blocker and diagnostic evidence without weakening the completion condition.
- Experimental backend policies or additional member modes, if shipped, have a separate contract and evidence.

## Phase 6 - Production candidate and community evidence

**Status:** Later

**Milestone:** `v1.0.0-beta.1`

Phase 6 validates installation, operation, troubleshooting, upgrades, and removal outside the original development environment.

### Repeatable test environments

- Run scheduled E2E in at least two independently operated Amphora clouds with documented version and network differences.
- Cover the required Service inputs in every conformance environment.
  Test each member mode where that mode is claimed.
- Include paths with and without a Floating IP and an HTTPS environment with Barbican enabled.
- Exercise controller restart, leader change, credential rotation, Octavia failure, version upgrade, rollback, finalization, and leak detection.
- Publish measured scale results and behavior under bursts of EndpointSlice updates, including OpenStack API call budgets.
- Publish Gateway creation and route update latency, Amphora instance and quota cost per Gateway topology, and the conditions that make direct Amphora a better fit than an Octavia L4 load balancer in front of an in-cluster proxy.

### Operations and security

- Publish metrics with bounded label values, alert guidance, dashboards, and useful Events.
- Keep controller control-plane metrics separate from traffic metrics exposed by Octavia or Amphora.
  Document which system is the source for each alert.
- Harden and validate the Phase 2 guidance for troubleshooting, recovery from blocked finalization, ownership audits, and checks before uninstalling.
- Document the minimum required Keystone roles for Octavia, Neutron, and Barbican, along with credential rotation and revocation.
- Complete a threat model, Kubernetes RBAC review, tests that secrets are redacted, and dependency and container vulnerability scanning.
- Test backup, restore, storage version migration, and rollback for every project CRD.

### Releases and installation

- Publish a versioned controller image for each supported architecture and an OCI Helm chart in addition to Kustomize manifests.
- Produce reproducible artifacts with checksums, SBOMs, provenance, and signatures under a documented identity and promotion policy.
- Test clean install, upgrade between supported minor versions, rollback, and safe uninstall from the exact release artifacts.
- Publish an Amphora compatibility matrix, limitations, support window, and upgrade notes for each release.

### Community evidence

- Maintain contributor, governance, reviewer, release, support, and security documentation with roles that reflect actual project responsibility.
- Keep design decisions and compatibility reports public.
  Maintain a reliable development setup and publish focused `good first issue` and `help wanted` tasks.
- Collect feedback and publishable upgrade evidence from at least two external adopters.
- Develop at least two release-capable maintainers from different organizations.
  Record independent review of real pull requests and design decisions, and make release and security response possible when one maintainer is unavailable.
- Submit the successful pinned profile report, reproduction instructions, and implementation metadata upstream for a Conformant listing.
  Use a Gateway API release eligible under the current upstream listing rules.
  Partial reports may document earlier progress but do not satisfy the project target or release gate.

An eventual Kubernetes SIG or OpenInfra home is a community decision, not an engineering milestone this repository can promise.
Consider that step only after the project has neutral governance, maintainers from more than one organization, adopter evidence, and sustained upstream participation.

### Phase 6 exit criteria

- Scheduled E2E and upgrade tests pass in two independent Amphora environments.
- Tests at the published scale limits and API budgets handle the measured endpoint churn without leaks or a burst of OpenStack API calls.
- Common failures are diagnosable without database access or private cloud knowledge.
- Release images and charts are reproducible, signed, scanned, and documented.
- All Core and declared Extended tests pass against the release artifacts, and an eligible, unmodified successful conformance report is published.
- No unresolved critical lifecycle or security defect blocks the release candidate.

## Phase 7 - Stable release

**Status:** Later

**Milestone:** `v1.0.0`

Stable means the supported contract can be operated and upgraded without depending on repository internals or maintainer intervention.

The stable milestone requires a Conformant upstream implementation listing and a successful report for the supported release configuration.
A documented development subset may be released earlier, but it does not satisfy the stable milestone.

The stable release requires all of the following:

- The class configuration API is at least `v1beta1`, with tested conversion, storage, defaulting, compatibility, and deprecation rules.
- Any experimental backend policy remains clearly versioned.
  A stable controller release does not make an experimental policy stable.
- At least two external adopters have completed an upgrade between supported minor versions and provided publishable evidence.
- At least two release-capable maintainers from different organizations have a record of independent review, and the release and security procedures do not depend on one person.
- The supported Kubernetes, Gateway API, OpenStack, Octavia, and CNI version policies and the release support window are public.
- The upstream conformance report and entry in the implementation list match the features the controller actually advertises.
- Scale limits, known limitations, troubleshooting, security and vulnerability response, and deprecation processes are public.
- Install, upgrade, rollback, credential rotation, and uninstall are reproducible using released artifacts.

Maintain full GATEWAY-HTTP Core conformance and pass all declared Extended features for each claimed Gateway API version.
If Amphora cannot express a required semantic safely, keep the milestone open until a supported solution is available.

## Deferred or out of scope

The following do not belong to the core roadmap unless a later proposal with supporting evidence changes the product contract:

- OVN or any non-Amphora Octavia provider
- Services of type `LoadBalancer`, Ingress, or migration of OCCM resources
- adoption of existing load balancers, Floating IPs, certificates, security groups, or other cloud resources
- creation or ownership of tenant networks, subnets, routers, and routes
- Traefik, Envoy, or another in-cluster proxy data plane
- TCPRoute, UDPRoute, TLSRoute, GRPCRoute, ListenerSet, GAMMA, service mesh, and multi-cluster Gateway
- managing several OpenStack projects or credential scopes from one controller deployment
- arbitrary annotations or extension fields that bypass a reviewed API
- HTTP transformation or traffic behavior that the public Octavia API cannot represent with Gateway API semantics
