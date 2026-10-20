# Amphora compatibility evidence

Amphora is the only provider accepted by the controller.
No controller release has a supported environment profile or Gateway API conformance report yet.
The [roadmap](../../ROADMAP.md) defines the evidence required for `probed`, `verified`, `supported`, and `conformant` claims.

## Development validation

Development runs have exercised HTTP traffic through a private VIP, Local NodePort backends, restart recovery, frozen class settings, sequential classes, two-worker member removal and restoration, and finalization with empty ownership audits.
A separate adapted `HTTPRouteSimpleSameNamespace` diagnostic also passed.
These results apply to their recorded source revisions and test harnesses.
They do not validate later changes or establish release compatibility.
Detailed run records remain private under the [test evidence policy](../reports/README.md).

The table distinguishes this development evidence from validation still needed for a support claim.
Unit tests, adapter tests, and envtest complement live runs but do not establish real cloud behavior.

## Evidence matrix

| Capability | Available evidence | Remaining live validation |
| --- | --- | --- |
| Load balancer, listener, and L7 lifecycle | Development E2E covers creation, HTTP traffic, converged no-op observation, restarts, and deletion. Adapter tests cover administrative state repair and recreation. | External drift, partial creation, and recovery after cloud failures. |
| NodePort members and health monitors | Development E2E covers Local endpoints and preserves an unchanged member during a two-worker transition. Local tests cover eligibility and event bursts. | Cluster traffic policy, Node lifecycle, traffic continuity, and release leak checks. |
| Floating IP allocation | The capability probe and adapter ownership tests pass. | Allocation, quota failure, detach recovery, and cleanup through the controller. |
| Class configuration | Development E2E covers frozen settings and sequential classes on the same topology. Envtest covers separate topologies using a fake provider. | Simultaneous Gateways using different class topologies. |
| Failure recovery | Development E2E covers leader replacement and controller restart. Local tests cover classified errors and finalizer retention. | Timeout, quota, rate limiting, Octavia failure, external deletion, and blocked finalization. |
| Gateway API bundle handling | Unit tests cover version mismatches and cleanup. Envtest uses the pinned Standard Channel bundle. | Bundle upgrades and recovery in a deployed cluster. |
| Gateway API conformance | One adapted upstream HTTP diagnostic passed. No profile report has been submitted. | Every Core test and declared Extended test in the target profile. See the [conformance plan](../testing-conformance.md). |

Network automation, terminated HTTPS, Barbican integration, and Pod IP members remain planned work.
See the [roadmap](../../ROADMAP.md) and [conformance feasibility review](../design/conformance-feasibility.md) for their design dependencies and known Amphora limitations.

## Reporting an environment

Use the OpenStack environment report issue template.
Identify the tested controller release or revision, relevant service versions, Amphora topology, network path, scope, and supporting evidence.
Mark unknown properties explicitly.
A compatibility claim applies only to that recorded artifact and environment.

Follow the [E2E guide](../testing-openstack-e2e.md) for execution and cleanup, and the [publication policy](../reports/README.md) for the public summary.
Add a supported environment profile here only when a reviewed public report provides the required evidence.
