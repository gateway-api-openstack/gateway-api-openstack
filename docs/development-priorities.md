# Current development priorities

The core Phase 3 features are implemented, with validation and release work remaining.
The [roadmap](../ROADMAP.md#work-carried-into-phase-3) tracks those gates, and the [architecture](design/architecture.md) records the controller boundaries.

## Run the upstream diagnostic and record gaps

Start with the [Gateway API v1.6.1 diagnostic](testing-conformance.md), which runs `HTTPRouteSimpleSameNamespace` with a NodePort backend and one HTTP Gateway.
The runner is available, but no live result has been recorded for this revision.
Its adapted fixture does not establish GATEWAY-HTTP conformance.
The default upstream setup also requires behavior outside the current implementation, so selecting a single test does not make that setup usable unchanged.

Record setup blockers and observed failures in the [gap report](reports/conformance-gap-template.md).
Distinguish controller defects, missing behavior, Octavia limitations, and environment problems.
ClusterIP backends, filters, backend weights, invalid backends, and ReferenceGrant combinations remain areas for the full profile analysis.
Keep `supportedFeatures` empty until the corresponding complete Core behavior passes the pinned suite.

## Revalidate the implementation in OpenStack

Follow the [OpenStack E2E guide](testing-openstack-e2e.md) to repeat traffic, status, restart, no-op, deletion, and leak checks against the selected Phase 3 revision.
The [earlier development run](providers/compatibility.md#development-validation) tested the Phase 2 baseline.
Extend the new run to class isolation, frozen configuration, worker membership changes, and recovery after an owned resource disappears.

Fault scenarios still need an executable procedure or injector as well as results from an approved dedicated environment.
The report fields alone do not exercise quota failures, timeouts, rate limiting, Octavia failures, or blocked finalization.

Member replacement currently removes obsolete members before creating replacements.
Replacing every member can leave an empty pool between reconciliations.
Local tests cover that state, but traffic continuity and a drain policy remain unverified.
Changing the order requires a transition design covering health, mixed backend traffic, quota, and rollback.

## Measure before increasing concurrency

Node event handling reuses dependency reads within each event.
Service and EndpointSlice handlers read routes sharing a parent once per event.
Tests feed Node and EndpointSlice bursts through the real predicates, queue, reconciler, and graph writer with a stateful fake provider.
They check bounded reads, coalescing, member removal and restoration, and zero mutations when membership is unchanged.
The writer still collects and validates fresh inputs under the Gateway UID lock.

Run the local benchmarks without cluster or cloud credentials:

```sh
go test -run '^$' -bench BenchmarkNodePortModel -benchmem ./internal/controller/httproute
go test -run '^$' -bench 'Benchmark(Node|EndpointSlice)EventFanout' -benchmem ./internal/controller/httproute
go test -run '^$' -bench BenchmarkWriterCoordination -benchmem ./internal/controller/graph
go test -run '^$' -bench BenchmarkGatewayGraphCalculation -benchmem ./internal/cloud/openstack/graph
```

Compare results on the same machine and toolchain.
The tests and benchmarks measure local work, and the burst tests advance provider progress without waiting for real cloud operations.
They do not establish cloud capacity or end-to-end latency.

The writer records lock wait, input collection, validation, and provider call durations.
The provider duration includes client-side waits and API work.
Separate rate limiter wait, Octavia pending duration, and total convergence measurements remain to be added or collected before increasing concurrency.

## Prepare artifacts for outside testing

The [release procedure](releasing.md) covers binary archives with matching CRDs, RBAC, Kustomize resources, and examples.
Choose a registry and publish an immutable pre-alpha image, then validate that artifact in OpenStack.
Scheduled E2E and its leak audit require an approved dedicated cluster, project, credentials, and quota.
Signatures, provenance, SBOMs, and a supported Helm chart remain later release work.

Review governance, support, security contact paths, repository settings, and release evidence before outside testing.
The [governance policy](../GOVERNANCE.md) describes independent review and maintainer growth.
