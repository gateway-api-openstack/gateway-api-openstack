# Amphora compatibility evidence

This page records test evidence for Amphora, the only provider accepted by the controller.
OVN and vendor providers are out of scope.

No controller release has a supported environment profile yet.
The Phase 0 probe tested individual Octavia and Neutron operations in one Amphora environment, but its exact versions and topology have not been published.
It did not test controller traffic, recovery, deletion, or Gateway API conformance.

Update this matrix when a reviewed public report supports a compatibility claim for a named controller revision and environment.
For routine development checks, follow the [test evidence policy](../reports/README.md).

The [roadmap](../../ROADMAP.md) defines the separate `probed`, `verified`, `supported`, and `conformant` evidence levels.

## Development validation

On 2026-10-09, a manual OpenStack E2E run passed all 12 required foundation checks against controller revision `221d9ba` with local test harness changes.
The run used selected NodePort backends with `externalTrafficPolicy: Local`, a private VIP, and no Floating IP.
It covered HTTP traffic, Gateway API status, leader recovery, a cold controller restart, converged metrics, deletion, and finalizer completion.
Ownership audits passed, cleanup returned to baseline, and the shared project's resource identities and compared settings were unchanged.

Five fault checks were `Not run`: external deletion of an owned child, blocked finalization, quota failure, request timeout and rate limiting, and Octavia resource failure.
A separate VIP diagnostic request timed out.
Its timing was not synchronized with teardown, so the cause remains unresolved.

The class API, graph writer, worker selection, dependencies, and test harness changed after this run.
It does not establish live validation of the Phase 3 revision.
Detailed records remain local under the [test evidence policy](../reports/README.md).
This development result does not establish a supported environment profile or Gateway API conformance.

## Local validation

Schema tests and API server envtest cover the class API and durable bindings.
The two-class envtest checks distinct frozen configurations after parameter reference swaps, parameter edits and deletion, and reconstruction with a fresh reconciler.
The envtest cloud provider is fake.

Node and EndpointSlice burst tests run the watch handlers, HTTPRoute reconciler, and graph writer against fake Kubernetes storage and a stateful fake provider.
They check member removal and restoration, preserve unchanged members, and require zero member changes when a burst ends at the existing desired state.
Mapper benchmarks separately count reads and fan-out.
These checks do not measure manager event delivery, real cloud convergence, or traffic continuity.

The [upstream HTTP diagnostic](../testing-conformance.md) selects the pinned `HTTPRouteSimpleSameNamespace` assertions with an adapted NodePort fixture.
Its local setup and cleanup tests do not execute the live diagnostic.
No OpenStack run or full conformance result is available for it.

## Evidence matrix

| Capability | Current evidence | Evidence required for a support claim | Known constraints |
| --- | --- | --- | --- |
| Load balancer and HTTP listener lifecycle | Phase 0 resource probe and [development E2E lifecycle checks](#development-validation) passed. Adapter tests cover listener recreation and administrative state repair | Initial creation, converged no-op, restart, drift, and E2E deletion with full ownership checks | One Amphora load balancer owned by each Gateway |
| L7 policies and rules | Phase 0 resource probe and [development E2E HTTP traffic](#development-validation) passed. Constrained controller unit tests exist | Real traffic, precedence, recovery after partial creation, and pinned conformance results | Only Gateway API semantics that Octavia represents exactly |
| NodePort members and health monitors | Phase 0 resource creation and [development E2E with Local NodePort backends](#development-validation) passed | Traffic for `externalTrafficPolicy: Cluster` and `Local`, endpoint churn, Node lifecycle, and leak checks | Selected Node addresses and NodePorts must be reachable from Amphora |
| Floating IP allocation | Phase 0 probe and adapter ownership tests passed | E2E tests with and without a Floating IP, quota failure, detach recovery, and cleanup | Only Floating IPs created by the controller. Existing addresses are not adopted |
| Reconciliation failure recovery | Earlier [E2E restart checks](#development-validation) passed. Local tests cover classified failures, ownership conflicts, finalizer retention, resource recreation, and restart from durable bindings | Restart, leader change, `PENDING_*`, timeout, quota, rate limit, external deletion, fault injection after partial creation, and the blocked finalization workflow in OpenStack | Ownership conflict stops mutation and uses a slower retry interval |
| Gateway API bundle handling | Unit tests cover exact, missing, and mixed CRD bundle versions, event filtering, mutation blocking, and cleanup during a mismatch. API server envtest loads the pinned Standard Channel CRDs and exercises the status subresource | Upgrade testing with the pinned Standard Channel manifests | This release accepts only the v1.6.1 bundle and does not advertise Gateway or HTTPRoute conformance |
| Class configuration and worker selection | [Local validation](#local-validation) covers isolated class settings and event-driven member convergence. Unit and adapter tests cover references, worker eligibility, and complete graph writes | E2E for the selected Phase 3 revision, multiple class topologies, parameter changes, worker membership transitions, and leak checks | Read-only topology acceptance does not prove write permissions, quota, backend connectivity, or conformance |
| Network and backend security automation | Not implemented | A documented network selection procedure and repeatable end-to-end tests for `Referenced`, `Unmanaged`, and any approved `Managed` mode | The controller does not own networks, subnets, routers, foreign security groups, or worker ports |
| Terminated HTTPS and Barbican | Not implemented | Tests for authorized Secret references, certificate creation and rotation, restart recovery, deletion, SNI, and orphan cleanup | Requires Barbican and a certificate lifecycle that verifies ownership before mutation or deletion |
| Pod IP members | Not implemented | Tests that cover CNI routability, EndpointSlice lifecycle, draining, security, and source IP behavior | If added, this mode will remain experimental and require explicit opt-in. NodePort remains the default mode with the broadest compatibility |
| Gateway API conformance | An adapted diagnostic runner exists but has not run against OpenStack. No report submitted | Unmodified report for the pinned Gateway API version with every claimed feature passing | Do not claim a conformance profile unless all of its Core features pass |

## Environment profile

A compatibility report must identify the controller artifact, relevant versions, Amphora topology, network path, tested scope, and accessible supporting evidence.
Mark unknown properties explicitly.
A compatibility statement applies only to that recorded release and environment.
“Works on OpenStack” is not sufficient evidence.

## Reporting an environment

Use the OpenStack environment report issue template.
A report must confirm Amphora and explain how Amphora reaches backend members.
Publish only information you are authorized to share.
Remove credentials, tokens, tenant identifiers, private addresses, customer names, and other sensitive topology details.

Follow the [E2E test guide](../testing-openstack-e2e.md) for prerequisites, project isolation, test scope, cleanup, and inventory checks.
Keep tenant inventories private and summarize the cleanup outcome and any limitations under the [test evidence policy](../reports/README.md).
Use the [conformance guide](../testing-conformance.md) to distinguish the adapted diagnostic from a full suite run and gap report.
