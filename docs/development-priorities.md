# Current development priorities

The Phase 3 features are implemented, with validation and release work remaining.
The [roadmap](../ROADMAP.md#work-carried-into-phase-3) tracks those gates, and the [architecture](design/architecture.md) records the controller boundaries.

## Resolve Core design gaps

Use the [conformance guide](testing-conformance.md) for current support and the [feasibility assessment](design/conformance-feasibility.md) for Octavia API blockers.
Phase 4 must establish the required Service forwarding, listener mapping, backend behavior, and certificate lifecycle designs before Phase 5 implements them.
Track unsupported API operations separately from controller defects and environment failures.

## Extend OpenStack validation

Use the [compatibility record](providers/compatibility.md#development-validation) to select coverage that is still missing.
Priorities are simultaneous isolation across different topologies, traffic continuity during member replacement, fault recovery, and blocked finalization.
Follow the [OpenStack E2E guide](testing-openstack-e2e.md) for preparation, execution, and cleanup.

Fault scenarios need reproducible procedures or injectors in an approved test environment.
Report fields alone do not exercise external deletion, quota exhaustion, rate limiting, timeouts, or Octavia failures.
Member replacement currently removes obsolete members before adding replacements.
A different order requires a design covering health, mixed traffic, quota, and rollback.

## Measure before increasing concurrency

The [architecture](design/architecture.md#kubernetes-events-and-measurements) describes indexed event handling and the writer's stage metrics.
Local burst tests and benchmarks measure reads, queue coalescing, and model calculation.
They do not establish cloud capacity or end-to-end latency.

Run the local benchmarks without cluster or cloud credentials:

```sh
go test -run '^$' -bench BenchmarkNodePortModel -benchmem ./internal/controller/httproute
go test -run '^$' -bench 'Benchmark(Node|EndpointSlice)EventFanout' -benchmem ./internal/controller/httproute
go test -run '^$' -bench BenchmarkWriterCoordination -benchmem ./internal/controller/graph
go test -run '^$' -bench BenchmarkGatewayGraphCalculation -benchmem ./internal/cloud/openstack/graph
```

Compare results on the same machine and toolchain.
Measure rate limiter wait, Octavia pending duration, and total convergence separately before increasing concurrency.

## Prepare artifacts for outside testing

The [release procedure](releasing.md) covers binary archives with matching CRDs, RBAC, Kustomize resources, and examples.
Choose a registry and publish an immutable pre-alpha image, then validate that artifact in OpenStack.
Scheduled E2E and its leak audit require an approved dedicated cluster, project, credentials, and quota.

Review governance, support, security contact paths, repository settings, and release evidence before outside testing.
The [governance policy](../GOVERNANCE.md) describes independent review and maintainer growth.
