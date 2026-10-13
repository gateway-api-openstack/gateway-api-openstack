# Roadmap

This roadmap sets the order of work for gateway-api-openstack. It is a planning
document, not a release promise. A feature moves forward only after its
ownership, failure behavior, and test evidence are clear.

The current development stage is **Phase 3 implementation complete, with release validation pending**.
The Phase 2 development milestone is complete for the current HTTP and NodePort reliability foundations, following a successful baseline OpenStack E2E run.
Phase 3 implements the complete Gateway graph writer, typed class configuration, worker member selection, and local scale checks.
A new OpenStack run, extended fault tests, the conformance gap report, and public image delivery remain validation and release work.
The project remains pre-alpha, with no production readiness, support, or conformance claim.

Each controller release pins the Gateway API version it supports. The current
baseline is the Gateway API v1.6.1 Standard Channel.

## Product contract

gateway-api-openstack is a Kubernetes Gateway API controller for HTTP and
terminated HTTPS traffic. It uses Octavia directly and supports only the
Amphora provider. It reconciles Gateway API resources into Octavia and Neutron
resources whose ownership can be verified before any mutation or deletion.
Terminated HTTPS also uses Barbican.

The goal is dependable behavior: predictable reconciliation, few unnecessary
OpenStack API calls, useful status, and safe upgrades. Application teams should
not need to understand OpenStack internals. The project does not aim to match
every feature or provider available in other cloud controllers.

The following boundaries are intentional:

- The controller accepts only the Amphora provider. Provider selection is not
  exposed to users, and every created load balancer must be verified as an
  Amphora load balancer. Release support remains limited to environments
  covered by published compatibility evidence.
- One Gateway owns one Octavia load balancer and its dependent graph.
- HTTP and terminated HTTPS are the data plane protocols in scope.
- By default, Amphora reaches backends through NodePorts when it cannot route
  directly to Pod IPs. Direct Pod IP members may be enabled later only for
  topologies whose routing has been verified.
- One controller deployment uses one OpenStack cloud, region, project, and
  credential scope. A project may run another deployment for another scope,
  with a distinct controller and cluster identity.
- The controller may resolve and validate existing networks, subnets, routers,
  ports, and security groups. It does not own the cloud network topology merely
  because it reads those resources.
- When a resource type is supported, the controller creates and manages only
  resources that belong to its Gateways: load balancers, listeners, pools,
  members, health monitors, L7 resources, Floating IPs, Barbican resources, and
  security resources covered by an accepted design.
- Existing load balancers, Floating IPs, certificates, and security groups are
  never adopted by the controller.
- Services of type `LoadBalancer` and every graph owned by the OpenStack Cloud
  Controller Manager (OCCM) remain the responsibility of
  cloud-provider-openstack.
- Ingress, proxy data planes, and other Octavia providers are not alternate
  modes of this controller.
- Gateway API behavior that Octavia cannot represent exactly is rejected in
  standard status. It is never silently ignored or approximated.

## Phase overview

The architecture document records the detailed ownership, identity, and
compiler rules. Accepted architecture decision records (ADRs) supersede its
proposed text. The table below summarizes each phase and its exit criteria.

| Phase | Status | Outcome | Exit gate |
| --- | --- | --- | --- |
| 0 — Feasibility | Historical | Prove the required Amphora primitives and ownership boundary | Capability probe retained, with later phases still requiring their ADRs |
| 1 — HTTP slice | Implemented and development-tested | One Gateway, listener, route, and NodePort backend | Formal compatibility claims still require published environment evidence |
| 2 — Reconciliation | Development milestone complete | Reliability foundations for the current HTTP and NodePort path | Baseline OpenStack E2E passed. Remaining work is carried into Phase 3 |
| 3 — Class API | Implemented locally | Typed topology configuration, one graph writer, and worker members | Revalidate the selected revision in OpenStack, publish conformance gaps, and prepare outside testing |
| 4 — Connectivity | Planned | Deterministic address, network, and security handling | Results from tests in OpenStack without adopting foreign network resources |
| 5 — HTTP/HTTPS | Planned | Multiple listeners, route compilation, and terminated HTTPS | Pinned conformance results and safe Barbican lifecycle |
| 6 — Production candidate | Later | Repeatable operations and community evidence | Two independent clouds, hardened releases, and an eligible conformance report |
| 7 — Stable | Later | Supported, upgradeable Amphora contract | Stable API, adopter upgrades, support policy, and release evidence |

Phase numbers describe engineering milestones, and version milestones are planning targets rather than published releases.
Closing Phase 2 changes the sequencing of its original gates.
The [work carried into Phase 3](#work-carried-into-phase-3) distinguishes completed implementation from outstanding release evidence.

## Near-term sequence

Phase 3 code is implemented and passes local unit, race, API server, and packaging checks.
The next work validates the implemented revision and identifies the remaining Gateway API gaps.

1. **Run conformance and record gaps.** Run the pinned GATEWAY-HTTP suite and classify its failures before expanding the HTTP compiler.
2. **Repeat OpenStack validation.** Test the class API, complete graph writer, worker selection, traffic, restart, finalization, and leak audit for the selected revision.
   Use the [OpenStack E2E test guide](docs/testing-openstack-e2e.md) and a dedicated environment for fault scenarios.
3. **Prepare outside testing.** Choose an artifact registry, publish an immutable pre-alpha image, and configure scheduled E2E only after a dedicated test environment and credentials are approved.

The detailed implementation and refactoring backlog is in
[docs/development-priorities.md](docs/development-priorities.md).

Community maturity is tracked separately from feature phases. The project
should build a record of independent reviews, public decisions, adopter tests,
and maintainers from more than one organization. An entry in the Gateway API
implementation list is a near-term ecosystem goal. A Kubernetes SIG or
OpenInfra home is a later community decision and is not promised by this
roadmap.

## Evidence and compatibility rules

Compatibility applies to a particular release and environment.
The canonical environment fields and report formats are in [docs/providers/compatibility.md](docs/providers/compatibility.md) and [docs/reports](docs/reports/README.md).
A report must identify the controller artifact, cloud and cluster versions, Amphora topology, network path, and exact tests well enough for another operator to understand its scope.
These requirements apply to formal compatibility and release claims.
For development runs, retain exact build and environment details locally and use a concise PR summary under the [test evidence policy](docs/reports/README.md).

Use evidence terms consistently. `Probed` means an API primitive worked in one
environment. `Verified` means a controller revision passed named tests in one
recorded environment. `Supported` means a release and environment are covered
by a maintained compatibility and support policy. `Conformant` means the
pinned upstream suite passed for the claimed profile.

`GatewayClass.status.supportedFeatures` is a runtime support declaration
consumed by the conformance suite, not a copy of an Octavia feature list or a
published conformance report. The controller publishes a feature only when
the corresponding behavior and every required feature combination have
passed the pinned Gateway API conformance suite. Declaring `Gateway` or
`HTTPRoute` means supporting that resource's full Core behavior. It cannot be
split into smaller feature names. A capability probe or a successful API call
is not enough.

Every phase must preserve the following invariants:

- the same desired state converges without further OpenStack mutations
- deletion can resume after a controller restart and cannot remove a foreign
  resource
- unsupported input is visible to the user before cloud mutation
- missing evidence is documented as missing rather than inferred
- public APIs remain compatible according to their declared maturity

## Phase 0 - Feasibility and ownership discovery

**Status:** Historical baseline — capability probe completed, design records
remain incomplete

**Milestone:** `phase-0-initialize`

Phase 0 established that an Amphora environment can create and inspect the
Octavia and Neutron primitives needed by the controller. It also established
the separate ownership boundary with cloud-provider-openstack and the
immutable identity requirements for safe deletion.

The retained probe is useful for environment discovery, but it is not a
controller end-to-end test. The architecture decisions originally identified
in this phase have not all been accepted as ADRs. Any unresolved decision is a
gate for the phase that depends on it. An implementation detail does not settle
the decision.

## Phase 1 - Constrained HTTP implementation

**Status:** Implementation complete — release evidence pending

**Milestone:** `v0.1.0`

Phase 1 implements the first end-to-end controller path:

- separate GatewayClass, Gateway, and HTTPRoute reconcilers built with
  controller-runtime and upstream Gateway API types
- selection by the exact controller name, with finalizers and stored identity
  for safe cleanup
- one HTTP listener, one selected route in the same namespace as the Gateway,
  one rule, and one NodePort Service backend in the route namespace
- exact hostname, Exact path, and PathPrefix matching by complete path elements
- one Octavia load balancer per Gateway, with an optional Floating IP created by
  the controller
- a pool, NodePort members, a health monitor, and L7 policies and rules for the
  selected HTTPRoute, with HTTPRoute identity included in ownership checks until
  the graph writer ADR is accepted
- standard status for the supported and explicitly rejected inputs
- a Gophercloud adapter boundary, unit tests, Kustomize deployment, example,
  and deterministic binary release packaging

The Phase 1 code is complete, and the [development E2E run](docs/providers/compatibility.md#development-validation) covered traffic, status, restart, converged no-op behavior, ordered deletion, and cleanup for the tested Local NodePort path.
A formal compatibility claim still requires a reviewed report for the exact release and documented Amphora topology.

## Phase 2 - Safe and efficient reconciliation

**Status:** Development milestone complete for the current HTTP and NodePort reliability foundations.

**Milestone:** `v0.2.0`

Phase 2 established the reliability baseline for the behavior implemented in Phase 1.
All 12 required foundations checks passed in a manual OpenStack run, including restart, no-op observation, finalization, and ownership audit.
The [validation record](docs/providers/compatibility.md#development-validation) identifies the tested revision and the checks that were not run.
The remaining implementation and evidence gates are carried into Phase 3 below.

### One writer for one Gateway graph

The complete graph writer is implemented in Phase 3 under [ADR 0001](docs/design/adr/0001-gateway-graph-writer.md).
Gateway and HTTPRoute reconcilers share one writer keyed by Gateway UID.
The writer rebuilds the complete graph from fresh Kubernetes inputs and durable bindings, then calls one provider operation.
The OpenStack adapter observes and validates the complete owned graph before executing at most one mutation.
Gateway and route component results keep status scoped to the resource whose behavior was evaluated.

The control loop follows these rules:

- Keep state for each reconciliation in local variables or a scope created for
  that request. Do not store loggers or objects in shared reconciler fields.
- Collect the Kubernetes and OpenStack state needed for one reconciliation
  before building the desired graph.
- Build and validate a deterministic desired graph without Gophercloud types.
- Keep model construction and diff calculation independent of I/O so they can be tested and benchmarked from input snapshots.
- Compare only the fields owned by this controller.
- Apply the smallest mutation in dependency order.
- Make at most one asynchronous state transition, observe it for a bounded
  period, and then return.
- Requeue work that is still progressing in Octavia instead of holding a
  worker in a long poll.

A coordinator keyed by Gateway UID may serialize graph writes within the active
leader process. It must not become a global mutex, and it is not a substitute
for a level-based desired graph that recovers after process or leader failure.

### Failure, drift, and deletion

Provider outcomes distinguish pending progress, authentication or
authorization failure, quota exhaustion, rate limiting, request timeout,
retryable service failure, terminal validation, resource failure, and
ownership conflict. The controller returns transient failures to the workqueue
for backoff. Failures that need operator action use a slower retry interval
with jitter, while ownership conflicts are checked less often. The `status`
field and Events use fixed messages rather than OpenStack response bodies. An
OpenStack HTTP response code alone is not enough. For example, a `404` may
mean safe recreation or completed deletion, depending on the observed role
and phase.

Tests and reconciliation must cover:

- a load balancer without its listener
- a listener or pool with only part of the expected route graph
- a successful create whose response is lost before a controller restart
- resources created by the controller and later deleted externally
- drift in fields owned by this controller without overwriting foreign fields
- `PENDING_*`, Octavia `ERROR`, conflict, quota, rate limiting, and timeout
  outcomes
- repeated deletion and finalization that can resume after a restart
- identity loss or collision, which must stop mutation rather than trigger
  adoption or destructive repair

After a Gateway or HTTPRoute converges, the controller observes its OpenStack
resources again on a configurable slow interval. Stable, bounded jitter derived
from the Kubernetes resource UID spreads these observations over time. The same
scheduling rule applies to Octavia resources that are still progressing.
Kubernetes watches remain responsible for changes to desired state.
Periodic observation finds changes made directly in OpenStack.

The OpenStack adapter recreates missing listeners, pools, members, and health
monitors after it verifies the remaining graph. It also enables a managed load
balancer or listener when its administrative state is changed outside the
controller. If route resources are present, the bound HTTPRoute first verifies
its exact identity, the complete Route graph, and the Gateway's provider,
subnet, listener, and Floating IP configuration. It then enables the listener
and load balancer in separate reconciliations. Gateway reconciliation performs
the repair directly only when there are no route resources.
Adapter tests cover these repairs.
Fault injection in an OpenStack environment remains part of the validation carried into Phase 3.

Failures during finalization keep the finalizer and stored binding. The
controller records a stable condition and emits an Event when the condition
changes. If an HTTPRoute no longer has a valid parent status entry, an
annotation owned by this controller stores a cleanup reason that contains no
sensitive data. This lets the controller emit the Event once without inventing
a ParentStatus. The annotation is removed when cleanup makes progress or the
binding is cleared. Unit tests cover these rules, including redaction of
provider error details. OpenStack fault tests and the operator recovery
procedure still need validation in a disposable Amphora environment. The
experimental [ownership audit](docs/design/ownership-audit.md) now compares
Kubernetes ownership records with OpenStack resources discovered in the
authenticated project without deleting anything. The command and report have
unit coverage. The documented
[operator recovery workflow](docs/operator-recovery.md) still needs validation
in that environment.

The controller envtest uses a real Kubernetes API server and etcd to cover
status subresources, stale resource versions, durable binding checkpoints,
progressing finalization with a newly constructed reconciler, and cache field
indexes. It does not exercise a manager or process restart, run an OpenStack
data plane, or replace the outstanding OpenStack fault tests.

### Kubernetes and OpenStack API efficiency

Manager setup indexes class parameters, GatewayClass ownership, parent Gateway references, backend Services, cleanup bindings, and EndpointSlice Services.
Watch handlers use indexed reads to find affected objects.
Parameter changes also enqueue unbound Gateways directly, including when the GatewayClass condition does not change.

Node events still begin with indexed routes that reference Services.
Within each event, the mapper reuses parent ownership, Service, and endpoint reads, then enqueues only affected managed NodePort routes.
Synthetic burst tests cover bounded dependency reads and queue coalescing.
Pure member and cloud graph benchmarks measure calculation separately from API calls.
The [local measurement guide](docs/development-priorities.md#measure-before-increasing-concurrency) provides reproducible commands without generating cloud load.

The writer exposes bounded stage metrics for lock wait, input collection, validation, and the provider call.
A provider call duration includes client-side waits and does not measure total Octavia convergence.
Keystone, Octavia, and Neutron continue to share one request budget and process-scoped clients.
Class topology checks make fresh reads, with reuse confined to one resolution call.
No cross-reconcile topology cache or independent member writer is introduced.

### Conformance feasibility and design gates

After completing Phase 3 implementation, run the pinned GATEWAY-HTTP suite as the next validation step and publish a gap analysis.
Classify every result as controller defect, compiler work, Octavia limitation, planned extension, or out of scope.
The known Core gaps include ClusterIP Services and any other Service backend accepted by Gateway API but rejected by the current NodePort path, `RequestHeaderModifier`, `RequestRedirect`, backend weights and invalid backend semantics, and every applicable ReferenceGrant combination.
Weights assigned to Services must not be inferred from Octavia member weights.

Do not advertise `HTTPRoute` in `supportedFeatures` until all HTTPRoute Core
behavior has passed. Gateway API does not provide a declaration for only this
controller's smaller HTTPRoute subset. `ReferenceGrant` is included in
`supportedFeatures` only after all combinations with the other reported
features pass. If NodePort remains the only stable backend path, report partial
GATEWAY-HTTP conformance.

The GatewayClass controller checks the bundle version annotation on every
installed Gateway API CRD. It reports `SupportedVersion=True` only when the
required CRDs are present and the complete bundle matches v1.6.1. A missing or
mixed annotation makes the class `Accepted=False` with reason
`UnsupportedVersion`. Reconciliation leaves an existing OpenStack graph in
place, blocks further provisioning and repair, and still allows deletion and
controller handoff cleanup. CRD watches resume normal reconciliation after the
installed bundle becomes supported again.

Both the CRD watch and mutation checks use metadata rather than downloading
every CRD schema. Mutation checks also confirm live controller ownership. A
slow retry on the GatewayClass lets it recover even when a brief CRD update does
not produce another watch event. Dependent objects use that fallback only when
they observe a live mismatch before the class status changes. HTTPRoute cleanup
rebuilds the current attachment, backend, endpoint, Node, and route selection
decision before it removes a graph that still has a managed parent.

[ADR 0001](docs/design/adr/0001-gateway-graph-writer.md) records the graph writer contract.
[ADR 0002](docs/design/adr/0002-class-configuration.md) records class configuration, frozen bindings, worker eligibility, and the alpha compatibility policy approved for this implementation.
[ADR 0003](docs/design/adr/0003-project-organization.md) sets the project organization, Go module, API group, and default controller identity.
Public image registry selection and independent release review remain open.

Any shared ownership of the `security_groups` field on worker Neutron ports requires a separate accepted ADR before that mode is implemented.
Keep the contribution guide, code of conduct, support policy, governance, OWNERS roles, and ADR process reviewed and aligned with actual project practice.

### Work carried into Phase 3

The carried work now has the following status.
Local tests do not replace OpenStack or conformance evidence.

| Work | Status and remaining gate |
| --- | --- |
| Complete Gateway graph writer and durable route lifecycle | Implemented with controller, adapter, race, and envtest coverage |
| Development identity, class API, frozen configuration, and worker selection | Implemented under ADRs 0001 and 0002, with schema and API server tests |
| Measure Node event handling and model calculation | Synthetic read budgets, coalescing tests, and local benchmarks added. Cloud scale limits are not established |
| Test intermediate backend transitions and recovery | Adapter tests cover one mutation per pass, unchanged members, replacement, pending state, and restart. Traffic continuity is not claimed |
| Exercise external deletion, quota, rate limiting, timeout, Octavia failure, and repeated deletion in OpenStack | Outstanding for the selected revision. Use a dedicated environment for fault injection |
| Validate blocked finalization and operator recovery with the ownership audit | OpenStack validation remains outstanding |
| Repeat baseline E2E and publish an environment report | Required before a compatibility claim for the new revision |
| Publish the pinned GATEWAY-HTTP gap analysis | Next validation step after Phase 3 implementation |
| Publish an immutable image and configure scheduled E2E with a leak audit | Registry and approved dedicated test infrastructure remain operational prerequisites |
| Review governance, support, ADR guidance, and contact paths | Required before outside release testing |

Preserve serialization by Gateway UID, reconcile state local to each request, bounded Octavia progress checks, and indexed dependency reads throughout this work.
A converged reconcile must issue zero OpenStack mutations and no semantic status patch.
Run unit, race, and envtest checks for the affected implementation, and repeat traffic, restart, finalization, audit, and leak checks for the selected live revision.
Formal release evidence follows the [publication policy](docs/reports/README.md).

## Phase 3 - Class configuration and topology API

**Status:** Implementation complete. OpenStack validation, conformance gap analysis, and outside release testing remain open.

**Milestone:** `v0.3.0`

Phase 3 replaces deployment-wide topology flags with the cluster-scoped OpenStackGatewayClassConfig API at `gateway-api-openstack.github.io/v1alpha1`.
GatewayClass requires a `parametersRef` naming that kind, with no namespace.
The controller identity defaults to `gateway-api-openstack.github.io/gateway-api-openstack` and can be overridden for an isolated installation.
The canonical repository and Go module are `github.com/gateway-api-openstack/gateway-api-openstack`.
[ADR 0003](docs/design/adr/0003-project-organization.md) records the move to the project organization and its API domain.
The API remains pre-alpha, and public image publication and supported upgrades remain separate release decisions.

The [class configuration reference](docs/reference/class-configuration.md) defines the schema, defaults, and operator workflow.
It includes VIP and member subnet selection, optional Floating IP allocation, Node address type, HTTP health monitor settings, and optional Amphora flavor and availability zone.
The API excludes credentials, project selection, provider choice, resource adoption, security group mutation, and arbitrary Octavia properties.
Amphora and NodePort remain the only data path.

OpenAPI and CEL validate the small schema without a webhook.
The Go types, DeepCopy methods, and CRD are maintained together and checked by schema tests and envtest.
There is no Kubebuilder scaffold or generated API tree.
Gateway infrastructure parameters and per-Gateway overrides remain unsupported.

A Gateway stores its resolved topology and class settings before the first cloud create.
That snapshot remains authoritative for the same Gateway UID through controller restart, class changes, parameter deletion, listener replacement, and recovery from an invalid spec.
It is released after finalization or controller handoff confirms that the owned graph is absent.
Changing the template does not move an existing load balancer to another network.

This development change does not provide a Phase 2 upgrade or rollback path, a flag compatibility window, or automatic conversion between API groups.
Clean up managed graphs with the controller that owns them before replacing an older development installation.
Future supported upgrades require their own compatibility policy and evidence.

### Worker NodePort member selection

The controller excludes Nodes carrying `node-role.kubernetes.io/control-plane` or the legacy `node-role.kubernetes.io/master` label.
A worker label is not required, and there is no control-plane opt-in.
It also honors `node.kubernetes.io/exclude-from-external-load-balancers`: an absent label or a value parsed as false allows eligibility, while true, empty, or invalid values exclude the Node.
Readiness, deletion, schedulability, and address checks still apply.

Both NodePort traffic policies use these rules.
Local additionally requires a ready endpoint on the Node.
Watches react to role, exclusion, endpoint, and address changes, including either supported Node address type across different classes.
Tests cover membership changes without recreating unchanged members.
Selecting only a few workers in a shared test environment still requires the E2E Local profile and workload placement labels.

### Deterministic resource resolution

A subnet or network reference selects exactly one UUID, exact name, or set of Neutron tags.
Tags form an AND selector.
Zero or multiple matches reject the configuration before provisioning.
Subnet resolution verifies the authenticated project, parent network, and compatible IP family.
An external network must be visible to the project and marked external, and may be shared from another project.
Reading those resources never grants ownership.

Existing Gateways use their snapshots instead of resolving mutable selectors again.
The controller does not create, alter, or delete tenant networks, subnets, routers, or worker ports.

### Amphora capability checks

Class acceptance checks the Octavia API, exact Amphora provider, required microversion, selected topology, and requested flavor or availability zone.
The base path requires microversion 2.5 for ownership tags.
Optional flavor and availability zone settings need higher microversions and read access to their profiles.
Unknown or unverifiable capabilities remain unavailable.

These checks perform reads only.
They do not prove permission to create resources, available quota, L7 data plane behavior, or Amphora-to-NodePort connectivity.
Those properties require the selected revision's OpenStack tests.
Class acceptance does not populate conformance `supportedFeatures`.

### Phase 3 validation and release gates

Local checks cover structural validation and defaults, multiple classes with separate topologies, frozen bindings, status, dependency watches, worker eligibility, restart, ownership conflicts, and one mutation per graph pass.
The release archives contain the matching CRD, RBAC, Kustomize resources, and examples alongside the binaries.

The following gates remain before claiming the complete Phase 3 release milestone:

- Repeat live E2E for the implemented class API and graph writer.
- Complete the fault and recovery evidence listed above.
- Run the pinned conformance suite and publish a gap analysis.
- Choose a registry and publish an immutable pre-alpha image for evaluation.
- Configure scheduled E2E and a leak audit in an approved dedicated environment.

No live result from the earlier controller revision proves these changes, and local measurements do not establish cloud capacity or traffic continuity.

## Phase 4 - Network, address, and security automation

**Status:** Planned

**Milestone:** `v0.4.0`

Phase 4 reduces the manual Neutron work needed for the NodePort path without
taking ownership of the cluster's network topology.

### Floating IP lifecycle

Phase 3 implements class modes `None` and `Allocate`, external network resolution, and cleanup of a proven detached Floating IP after its load balancer disappears.
It also detects detachment while the load balancer exists and stops provisioning to prevent duplicate allocation.
Phase 4 completes the remaining connectivity and recovery behavior:

- Recover an owned Floating IP that becomes detached while its load balancer still exists.
- Validate external deletion, quota failure, and address recovery with live traffic tests.
- Report failures in the VIP or Floating IP network path with useful diagnostics.
- Preserve the rule that existing addresses are never adopted and foreign addresses on a VIP port cause an ownership conflict.

### Frontend security groups

`OctaviaManaged` remains the default: the controller omits `vip_sg_ids` and
Octavia manages listener access. A `Referenced` mode may pass security groups
supplied by the user through `vip_sg_ids` only after the controller verifies API
support. The controller validates the groups and does not change their rules.

Custom VIP security groups were added in Octavia API microversion 2.29 with
Octavia 16.0 (OpenStack 2025.1). They are applied to the VIP and Amphora VRRP
ports. Octavia retains its internal VRRP and HAProxy peer rules but no longer
manages listener ingress rules. The option is incompatible with listener
`allowed_cidrs` and SR-IOV load balancers. Those consequences must be
validated and visible before a Gateway is programmed. The currently pinned
Gophercloud version does not expose `vip_sg_ids` in its typed create options,
so implementation also requires an appropriate dependency update or a narrow,
tested extension. The initial implementation will rely on frontend security
groups managed by Octavia and will not create a separate frontend group.

### Backend security groups

Start with `Referenced` and `Unmanaged` modes. `Referenced` validates the
configuration supplied by the operator without modifying the referenced group.
`Unmanaged` leaves backend security to the environment. API discovery can
validate structural prerequisites, but it cannot prove that Amphora members can
reach NodePort backends. Octavia health status, an active reachability check, or
end-to-end test results from OpenStack must demonstrate that connectivity.

A `Managed` mode may be introduced only after its ADR defines a narrow shared
ownership contract. Worker Neutron ports belong to CAPO, another controller,
or the operator. Changing their `security_groups` list is a mutation of a
foreign object even when this controller created the group being attached.

If accepted, managed mode follows these rules:

- Enable it only through an explicit class opt-in.
- Create only groups and rules tagged with the controller's complete ownership
  identity.
- Map each Node provider ID to its Nova server UUID and selected address. Then
  validate exactly one Neutron port in the authenticated project by
  `device_owner`, `device_id`, selected fixed IP and subnet, and
  `port_security_enabled`.
- Preserve every foreign security group association. Add or remove only the
  group ID added by this controller.
- Include the observed Neutron revision in each update where the API supports
  it. If the port changes concurrently, reject the mutation and observe the
  port again before retrying.
- Reconcile only NodePorts still referenced by managed routes.
- Use exact ports by default. Opening the whole NodePort range requires an
  explicit choice between quota use and network exposure.
- Take backend source CIDRs from explicit configuration or verified
  environment evidence. Do not assume the VIP subnet is the source.
- Define the identity and reference counting for a shared group, along with
  restart recovery, uninstall, and rollback behavior.

Both `externalTrafficPolicy: Cluster` and `externalTrafficPolicy: Local` need
separate tests because their member selection differs. Node addition, removal,
address change, readiness, and EndpointSlice changes must converge the member
set and security access together without causing a burst of OpenStack API calls.

### Diagnostics

Add a read-only diagnostic command or dedicated binary for authentication,
Amphora and microversion checks, network resolution, mapping from each Node to
a Neutron port, NodePort reachability prerequisites, permissions to read and
update security groups, Floating IP quota, and optional Barbican access.
Diagnostic output must be redacted and suitable for attaching to an issue.

Compatibility work in this phase records IPv4, IPv6, or dual-stack behavior
instead of assuming the IP family. It also records Amphora topology and the
tested kube-proxy implementation or replacement. Unsupported combinations are
reported as such before they become a release promise.

### Phase 4 exit criteria

- Network selection never chooses arbitrarily and does not mutate OpenStack
  when resolution is ambiguous.
- The controller safely recovers from Floating IP detachment and from drift in
  security rules that it created.
- Frontend `OctaviaManaged` and `Referenced` modes and backend `Referenced`
  and `Unmanaged` modes have documented results from an OpenStack environment.
- If `Managed` backend security is enabled, Node lifecycle and concurrent port
  changes pass E2E without changing or removing foreign groups or rules.
- The ADR for modifying worker ports owned by another component decides whether
  managed backend security will be supported. If managed mode is accepted, the
  tests must show that NodePort backends are reachable without manually
  changing the rules on each port. Otherwise, the operator remains responsible
  for the referenced configuration.
- No network, subnet, router, CAPO resource, or OCCM resource is adopted or
  deleted.
- The recurring environment job exercises both claimed NodePort traffic
  policies, endpoint changes, and leak detection without an unbounded burst of
  Kubernetes or OpenStack API calls.

## Phase 5 - Complete the Amphora HTTP/HTTPS path

**Status:** Planned

**Milestone:** `v0.5.0`

Phase 5 adds Gateway API features after graph reconciliation and connectivity
are stable.

### Listener and route compiler

Support multiple Gateway listeners, multiple attached HTTPRoutes, multiple
rules, and multiple matches. Enforce `allowedRoutes`, namespace selectors,
`parentRef.sectionName`, and accurate `attachedRoutes` status.

The compiler owns the entire listener L7 graph. Gateway API matches in a rule
are alternatives, while conditions inside a match are conjunctive. Octavia L7
policies are ordered alternatives whose rules are conjunctive. All attached
routes therefore contribute to one deterministic policy order. Precedence,
tie-breaking, and Octavia policy positions must be stable across restarts and
map iteration.

Gateway API PathPrefix matches complete path elements, while Octavia
`STARTS_WITH` is a raw string prefix. `/foo` continues to compile into an exact
`/foo` policy and a `/foo/` prefix policy so it does not match `/foobar`.

If several Gateway listeners share a port, the controller may group them onto
one physical Octavia listener only when protocol, TLS, hostname, certificate,
and status semantics have been proven equivalent. Otherwise the unsupported
listener combination is rejected with standard listener conditions rather
than partially programmed.

### Supported HTTP behavior

Start with exact and wildcard hostnames, Exact and PathPrefix paths, default
routes, and exact header matches where the Amphora API can represent them
precisely. Request redirects may be added only for combinations whose scheme,
host, port, path, query, and HTTP status semantics match Gateway API.

Method and query matching, request or response header modification, URL
rewrite, request mirroring, arbitrary `ExtensionRef`, and timeouts or retries
configured for individual routes remain unsupported unless a later Octavia API
offers an exact mapping.
Unsupported or incompatible fields receive standard Gateway API conditions.

Weighted backends require the conformance feasibility decision carried into Phase 3.
Weights assigned to Services, different endpoint counts, weight zero, and Gateway API behavior when a backend is invalid must match the specification.
Similar Octavia member weights are not enough to prove this behavior.
If exact semantics cannot be demonstrated, the feature remains rejected and the project publishes partial rather than full conformance.

### Cross-namespace references

Add ReferenceGrant handling for Service and Secret references. Removal of a
grant must revoke the reference and safely converge the dependent graph. Include
`ReferenceGrant` in `supportedFeatures` only after every applicable combination
with the controller's other reported features passes conformance.
Attachment of a route in another namespace to a Gateway follows Gateway API's
`allowedRoutes` rules and is not treated as a generic object reference grant.

### Terminated HTTPS

Support terminated HTTPS from Kubernetes TLS Secrets through Barbican resources
created by the controller. Start with one default certificate per listener. Add
SNI certificate sets after creation, rotation, rollback, and cleanup have been
tested.

The TLS design must cover Secret validation, redaction of private keys and
certificate data, cross-namespace authorization, immutable identity for
Barbican resources,
certificate rotation without downtime, crash recovery between old and new
certificates, and deletion of only the secrets and containers created by the
controller. If Barbican cannot expose enough identity to prove ownership, the
controller does not manage that resource type until a safe identity mechanism
has been designed and tested.

Barbican is required only for a release profile that claims terminated HTTPS.
An HTTP-only environment without Barbican can remain a verified or supported
profile if the release contract and compatibility matrix say so explicitly.

### Addresses and backend policy

Support representable `Gateway.spec.addresses` requests as Octavia VIP
addresses: dynamic allocation or a requested IP inside the validated VIP
subnet. Reject an unsupported address type, an address outside the binding, or
an address already in use. The class Floating IP policy remains a separate
choice for the external address. The API contract must state whether `Allocate`
adds a new FIP to a requested VIP or rejects that combination before mutation.

Introduce a namespaced OpenStackBackendPolicy only when documented use cases
require health monitor settings for individual Services. If added, the first
version will be an experimental `v1alpha1` Direct Policy Attachment. It will
use a `targetRef` to a Service in the same namespace, or to a named Service port
through `sectionName`, and contain only health monitor settings. The CRD carries
the `gateway.networking.k8s.io/policy: Direct` label and reports standard
`Accepted`, `Conflicted`, and `TargetNotFound` reasons through
`PolicyAncestorStatus` entries for each Gateway and controller. Session
persistence, pool algorithms, and connection limits do not belong in the first
version without separate evidence.

Support for direct Pod IP members remains optional and is not required for
`v1.0`. Enabling it requires checks that Amphora can route to Pod IPs, correct
handling of EndpointSlice ready, serving, and terminating conditions, safe
draining, CNI compatibility evidence, explicit security controls, and
documentation of source IP behavior. NodePort remains the default mode with the
broadest compatibility.

### Phase 5 exit criteria

- Multiple HTTP and terminated HTTPS listeners and routes share one Gateway
  without ownership conflicts or unstable policy order.
- Route precedence and PathPrefix matching by complete path elements pass unit
  tests, conformance tests, and tests in OpenStack.
- Cross-namespace Service and Secret access is granted and revoked exactly as
  Gateway API specifies.
- Certificate creation, rotation, restart recovery, and deletion leave no
  orphaned Barbican resources.
- Every reported `supportedFeatures` entry has a matching pinned conformance
  result.
- Any Core behavior that Amphora cannot represent is recorded openly. The
  release does not claim full GATEWAY-HTTP conformance in that case.
- Experimental backend policy or Pod IP support, if shipped, is clearly
  separated from the stable NodePort contract and has its own evidence.

## Phase 6 - Production candidate and community evidence

**Status:** Later

**Milestone:** `v1.0.0-beta.1`

Phase 6 validates installation, operation, troubleshooting, upgrades, and
removal outside the original development environment.

### Repeatable test environments

- Run scheduled E2E in at least two independently operated Amphora clouds with
  documented version and network differences.
- Cover NodePort in every environment. Test Pod IP only where that mode is
  claimed.
- Include paths with and without a Floating IP and an HTTPS environment with
  Barbican enabled.
- Exercise controller restart, leader change, credential rotation, Octavia
  failure, version upgrade, rollback, finalization, and leak detection.
- Publish measured scale results and behavior under bursts of EndpointSlice
  updates, including OpenStack API call budgets.
- Publish Gateway creation and route update latency, Amphora instance and quota
  cost per Gateway topology, and the conditions that make direct Amphora a
  better fit than an Octavia L4 load balancer in front of an in-cluster proxy.

### Operations and security

- Publish metrics with bounded label values, alert guidance, dashboards, and
  useful Events.
- Keep controller control-plane metrics separate from traffic metrics exposed
  by Octavia or Amphora. Document which system is the source for each alert.
- Harden and validate the Phase 2 guidance for troubleshooting, recovery from
  blocked finalization, ownership audits, and checks before uninstalling.
- Document the minimum required Keystone roles for Octavia, Neutron, and
  Barbican, along with credential rotation and revocation.
- Complete a threat model, Kubernetes RBAC review, tests that secrets are
  redacted, and dependency and container vulnerability scanning.
- Test backup, restore, storage version migration, and rollback for every
  project CRD.

### Releases and installation

- Publish a versioned controller image for each supported architecture and an
  OCI Helm chart in addition to Kustomize manifests.
- Produce reproducible artifacts with checksums, SBOMs, provenance, and
  signatures under a documented identity and promotion policy.
- Test clean install, upgrade between supported minor versions, rollback, and
  safe uninstall from the exact release artifacts.
- Publish an Amphora compatibility matrix, limitations, support window, and
  upgrade notes for each release.

### Community evidence

- Maintain contributor, governance, reviewer, release, support, and security
  documentation with roles that reflect actual project responsibility.
- Keep design decisions and compatibility reports public. Maintain a reliable
  development setup and publish focused `good first issue` and `help wanted`
  tasks.
- Collect feedback and publishable upgrade evidence from at least two external
  adopters.
- Develop at least two release-capable maintainers from different
  organizations. Record independent review of real pull requests and design
  decisions, and make release and security response possible when one
  maintainer is unavailable.
- Submit a pinned conformance report upstream only when the submission can
  record partial results without overstating runtime `supportedFeatures`.
  Otherwise retain the public gap report until Core support makes an accurate
  submission possible. Under the current upstream listing policy, an
  implementation needs at least a partially conformant report from an accepted
  recent Gateway API release to join the implementation list.

An eventual Kubernetes SIG or OpenInfra home is a community decision, not an
engineering milestone this repository can promise. Consider that step only
after the project has neutral governance, maintainers from more than one
organization, adopter evidence, and sustained upstream participation.

### Phase 6 exit criteria

- Scheduled E2E and upgrade tests pass in two independent Amphora
  environments.
- Tests at the published scale limits and API budgets handle the measured
  endpoint churn without leaks or a burst of OpenStack API calls.
- Common failures are diagnosable without database access or private cloud
  knowledge.
- Release images and charts are reproducible, signed, scanned, and documented.
- A conformance report for the release is published, with every skip or
  failure explained.
- No unresolved critical lifecycle or security defect blocks the release
  candidate.

## Phase 7 - Stable release

**Status:** Later

**Milestone:** `v1.0.0`

Stable means the supported contract can be operated and upgraded without
depending on repository internals or maintainer intervention.

The stable milestone keeps eligibility for the upstream implementation list as
a gate. A useful, documented pre-stable subset may be released earlier, but it
must not be called the stable Gateway API contract if it cannot submit the
required current report.

The stable release requires all of the following:

- The class configuration API is at least `v1beta1`, with tested conversion,
  storage, defaulting, compatibility, and deprecation rules.
- Any experimental backend policy remains clearly versioned. A stable
  controller release does not make an experimental policy stable.
- At least two external adopters have completed an upgrade between supported
  minor versions and provided publishable evidence.
- At least two release-capable maintainers from different organizations have a
  record of independent review, and the release and security procedures do not
  depend on one person.
- The supported Kubernetes, Gateway API, OpenStack, Octavia, and CNI version
  policies and the release support window are public.
- The upstream conformance report and entry in the implementation list match the
  features the controller actually advertises.
- Scale limits, known limitations, troubleshooting, security and vulnerability
  response, and deprecation processes are public.
- Install, upgrade, rollback, credential rotation, and uninstall are
  reproducible using released artifacts.

Full GATEWAY-HTTP Core conformance is desirable because it gives users the clearest portable contract.
If the conformance feasibility work proves that Amphora cannot express a Core semantic safely, the stable contract will say so, publish the conformance results, including failures, and remain useful within its documented HTTP/HTTPS subset without overstating portability.

## Deferred or out of scope

The following do not belong to the core roadmap unless a later proposal with
supporting evidence changes the product contract:

- OVN or any non-Amphora Octavia provider
- Services of type `LoadBalancer`, Ingress, or migration of OCCM resources
- adoption of existing load balancers, Floating IPs, certificates, security
  groups, or other cloud resources
- creation or ownership of tenant networks, subnets, routers, and routes
- Traefik, Envoy, or another in-cluster proxy data plane
- TCPRoute, UDPRoute, TLSRoute, GRPCRoute, ListenerSet, GAMMA, service mesh, and
  multi-cluster Gateway
- managing several OpenStack projects or credential scopes from one controller
  deployment
- arbitrary annotations or extension fields that bypass a reviewed API
- HTTP transformation or traffic behavior that the public Octavia API cannot
  represent with Gateway API semantics

## Technical references

- [Gateway API implementer's guide](https://gateway-api.sigs.k8s.io/guides/implementers-guide/)
- [GatewayClass parameters](https://gateway-api.sigs.k8s.io/reference/api-types/gatewayclass/)
- [Per-Gateway infrastructure (GEP-1867)](https://gateway-api.sigs.k8s.io/geps/gep-1867/)
- [Gateway API policy attachment](https://gateway-api.sigs.k8s.io/reference/policy-attachment/)
- [Gateway API implementation listing](https://gateway-api.sigs.k8s.io/docs/implementations/list/)
- [Gateway API v1.6.1 release](https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.1)
- [Gateway API conformance tests](https://github.com/kubernetes-sigs/gateway-api/tree/main/conformance)
- [Octavia API v2](https://docs.openstack.org/api-ref/load-balancer/v2/)
- [Octavia custom VIP security groups](https://docs.openstack.org/octavia/latest/contributor/specs/version15.0/custom-security-groups-for-VIP-ports.html)
- [AWS Load Balancer Controller design](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/how-it-works/)
- [AWS Load Balancer Controller security groups](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/deploy/security_groups/)
- [Gophercloud](https://github.com/gophercloud/gophercloud)
- [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)
- [cloud-provider-openstack](https://github.com/kubernetes/cloud-provider-openstack)
- [Kubernetes repository guidelines](https://github.com/kubernetes/community/blob/main/github-management/kubernetes-repositories.md)
- [Kubernetes template project](https://github.com/kubernetes/kubernetes-template-project)
