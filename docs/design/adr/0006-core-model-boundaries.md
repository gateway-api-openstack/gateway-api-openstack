# ADR 0006: Core model and policy boundaries

Status: Accepted for development

Date: 2026-11-06

Owner: `jihyun-huh`

The repository owner approved these scoped directions after reviewing other Gateway API implementations.
This records project approval, not independent review, implemented support, or conformance evidence.
[ADR 0005](0005-core-resource-mapping.md) retains the unresolved physical mappings and their verification gates.
[ADR 0007](0007-direct-endpoint-members.md) subsequently permits an explicit Endpoint mode implementation while network verification remains pending.

## Context

The next implementation needs to accept more Service inputs, combine routes, and preserve Service weights without expanding cloud ownership implicitly.
The controller must also scale across Gateways without letting independent reconcilers mutate the same load balancer.
These boundaries can be decided before selecting every Octavia mapping or certificate lifecycle mechanism.

## Decision

### Support explicit forwarding environments

Keep the NodePort contract from [ADR 0002](0002-class-configuration.md).
Develop direct endpoint forwarding first for a documented network environment whose CNI, routing, security, and address families permit traffic in both directions between Amphora and the endpoints.
The exact initial environment remains to be selected and verified.
A member creation response does not establish that capability.

Resolve the required ClusterIP, headless, and manually managed EndpointSlice inputs without changing user Services.
Do not infer direct endpoint reachability from NodePort success or silently fall back by changing worker ports, tenant routes, or OCCM resources.
Additional environments require their own evidence.
This decision does not enable a new member mode or define its public configuration fields.

### Keep one mutation boundary per Gateway

[ADR 0001](0001-gateway-graph-writer.md) remains the writer contract.
Build the logical listener and route model for the complete Gateway before assigning physical resources and policy order.
Retain logical listener status and exact route ownership when physical resources are shared.
The additional identity keys and cross-namespace inventory strategy remain acceptance gates in ADR 0005.

One logical writer owns each Gateway graph, not all Gateways globally.
Different Gateway graphs may reconcile concurrently through bounded workers sharing the Gateway UID coordinator and authenticated OpenStack clients.
The process shares one request budget for its project rather than assigning a separate budget to each worker.
Same-Gateway events from different reconcilers still require the common coordinator.
Do not mutate a Gateway's graph while its load balancer reports `PENDING_*`.

Replicas use leader election for availability.
Increasing worker concurrency requires race tests and measurements of queue latency, graph lock wait, API calls, and throttling.
Active controller sharding requires a separate ADR covering disjoint ownership, dependency watches, finalization, handoff, in-flight requests, and the aggregate project request budget.
A process-local lock or Kubernetes Lease alone does not fence an old writer's OpenStack requests.
Independent writers must not mutate the same load balancer.

### Preserve backend groups

The desired model keeps Service backend selection separate from endpoint membership.
Each backend retains its reference, authorization result, weight, protocol, applicable policy, and endpoint set.
An invalid reference remains distinct from a valid Service with no eligible endpoints or failed health checks.
Do not discard an invalid backend's required error share when another backend is valid.

This selects the logical model, not one pool per Service or a single combined pool.
Choose the Octavia mapping only after the weight and failure cases in ADR 0005 establish its behavior.
Pool sharing across routes, endpoint deduplication, and cross-Gateway sharing are not authorized by this decision.

### Start with direct backend policy attachment

The first backend policy uses Direct Policy Attachment for a Service in the policy's namespace, optionally narrowed to a named Service port.
Keep its initial purpose to backend health monitor settings as planned in the roadmap.
Do not add a hierarchy of defaults and overrides or field-by-field policy merging to the first version.
Core routing behavior stays in the standard Gateway API fields.

For conflicting policies at the same effective target, use creation time followed by namespace/name to choose one policy and report the others as conflicted.
The API design must settle whole-Service and port-specific overlap before implementation.
Report the effective result per Gateway and controller, and ensure a policy cannot change a shared cloud resource in a way that affects unrelated routes.
The policy grants no authority to modify the referenced Service, worker ports, or security group rules.
The CRD schema, defaulting, validation, and exact status contract still need review before adding the API.

## Alternatives and consequences

A universal endpoint mode would hide routing assumptions that differ between clouds.
Flattening every endpoint into one weighted list would discard information needed to preserve Service-level behavior.
Independent route writers would lose the complete policy order and compete for an asynchronous load balancer.
A general inherited policy engine adds conflict and authorization rules that the initial health monitor use case does not require.

The selected boundaries allow compiler and reference work to proceed while physical mappings remain under review.
They do not authorize certificate publication, external certificate references, a new controller identity, cloud operations, or a new support claim.
Existing bindings, ownership checks, and cleanup ordering remain in force.

## Verification and follow-up

- Prove the selected forwarding environment with the required Service inputs, endpoint lifecycle, port resolution, and each claimed address family.
- Verify whole-Gateway ordering, partial validity, cross-namespace inventory completeness, and cleanup after individual routes or listeners disappear.
- Test concurrent changes to different Gateways and serialized parent and route changes to the same Gateway, including restart and leader handoff.
- Select a pool mapping using traffic distributions, weight zero, overlapping endpoints, health changes, and mixed invalid and empty backends.
- Review the direct policy API and test conflicts, target deletion, port overlap, status, and isolation from unrelated routes.
- Resolve certificate transport and ownership separately before enabling HTTPS publication.

The [roadmap](../../../ROADMAP.md) sequences these tasks.
No migration or rollout mechanism is introduced by this design record.

## References

- [AWS Load Balancer Controller concurrency and request budgets](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/deploy/scaling/).
- [Cluster API controller tuning](https://main.cluster-api.sigs.k8s.io/developer/core/tuning).
- [Envoy Gateway backend groups](https://github.com/envoyproxy/gateway/blob/v1.9.3/internal/xds/translator/cluster.go#L1019).
- [GKE direct policy configuration](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-gateway-resources).
- [Kubernetes leader election and fencing limits](https://github.com/kubernetes/client-go/blob/v0.36.0/tools/leaderelection/leaderelection.go).
