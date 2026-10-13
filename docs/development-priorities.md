# Current development priorities

Status: Phase 3 implementation complete, with validation and release work remaining.

The class configuration API, complete Gateway graph writer, frozen bindings, worker member selection, and local event handling measurements are implemented.
[ADR 0001](design/adr/0001-gateway-graph-writer.md) and [ADR 0002](design/adr/0002-class-configuration.md) record the decisions approved for this development change.
The [roadmap](../ROADMAP.md#work-carried-into-phase-3) separates completed code from evidence still required for a release.

## Run conformance and record gaps

Run the pinned Gateway API v1.6.1 GATEWAY-HTTP suite as the next validation step.
Publish a concise gap analysis even when failures are expected.
Classify failures as controller defects, missing compiler behavior, backend limitations, Octavia API limitations, or intentional non-goals.

Known areas to evaluate include ClusterIP Services, request header modification, request redirects, backend weights, invalid backends, and ReferenceGrant combinations.
Do not translate Service backend weights into Octavia member weights without proving equivalent semantics.
The result should guide the HTTP compiler work after Phase 3.

A development gap report is separate from an upstream conformance submission.
Keep `supportedFeatures` empty until the corresponding complete Core behavior passes the pinned suite.
Use the [gap report template](reports/conformance-gap-template.md) and [test evidence policy](reports/README.md).

## Revalidate the implementation in OpenStack

Repeat the baseline traffic, status, restart, no-op, deletion, and leak checks against the selected Phase 3 revision.
The [earlier development run](providers/compatibility.md#development-validation) tested a different controller and does not establish this revision's behavior.
The E2E runner now creates a typed class configuration and requires the operator to install its CRD first.
Follow the [OpenStack E2E guide](testing-openstack-e2e.md).

Extend validation to multiple classes, frozen configuration after parameter changes, worker membership changes, and recovery when an owned resource disappears.
Test quota failures, timeouts, rate limiting, Octavia failures, and blocked finalization only in a dedicated environment approved for fault injection.
Keep exact inventories, cloud details, logs, and image digests local unless a formal compatibility report requires them.

Backend replacement is not atomic.
The current member plan removes obsolete members before creating replacements, so replacing every member can leave an empty pool between reconciliations.
Local tests cover this intermediate graph, but they do not establish traffic continuity or a safe drain policy.
Any change to that ordering needs a transition design covering health, mixed backend traffic, quota, and rollback.

## Measure before increasing concurrency

The controller now reuses dependency reads within one Node event.
Synthetic tests verify bounded reads for routes sharing Gateways and Services, unchanged enqueue behavior, fresh reads on later events, and workqueue coalescing.
This reuse does not cross reconciliation boundaries or authorize cloud mutation.
The graph writer still rebuilds and validates fresh inputs under the Gateway UID lock.

Run local benchmarks without Kubernetes or OpenStack credentials:

```sh
go test -run '^$' -bench BenchmarkNodePortModel -benchmem ./internal/controller/httproute
go test -run '^$' -bench BenchmarkNodeEventFanout -benchmem ./internal/controller/httproute
go test -run '^$' -bench BenchmarkWriterCoordination -benchmem ./internal/controller/graph
go test -run '^$' -bench BenchmarkGatewayGraphCalculation -benchmem ./internal/cloud/openstack/graph
```

Compare results on the same machine and toolchain.
The benchmarks separate pure calculation from dependency reads and coordination.
They do not establish OpenStack capacity, production latency, or safe worker counts.

The writer's stage histogram measures lock wait, Gateway inputs, route inputs, validation, and the complete provider call.
The provider stage includes client-side waits and API work within that call.
Use existing OpenStack request metrics for API latency and throttling.
Pending duration and total cloud convergence still need environment measurements.
Keep object names, UIDs, and cloud IDs out of metric labels.

## Prepare artifacts for outside testing

Binary release archives now contain matching CRDs, RBAC, Kustomize resources, and examples.
The [release procedure](releasing.md) covers checksum and extracted-manifest checks.
The repository still does not publish a controller image or run scheduled live E2E.

Choose a registry and an immutable pre-alpha image reference, then validate that artifact in OpenStack.
Configure scheduled E2E and the post-test leak audit only after a dedicated Kubernetes cluster, OpenStack project, credentials, and quota are approved.
Distinguish infrastructure failures from controller regressions in the job result.
Signatures, provenance, SBOMs, and a supported Helm chart remain later release work.

## Preserve the implemented boundaries

Keep Kubernetes types in controller packages and Gophercloud types in the OpenStack adapter.
Class configuration contains infrastructure choices, while credentials and process identity stay in deployment configuration.
Both Gateway and HTTPRoute reconciliation use the same graph writer.
There is no independent member writer or process-local fragment registry.
See the [development layout](development-layout.md) for package responsibilities.

The current API is a development alpha with no Phase 2 migration or rollback promise.
Changes to the API group, controller identity, or frozen infrastructure need an explicit compatibility decision before a supported release.
Do not broaden route selection, resource sharing, or cleanup authority as an incidental refactor.

## Work that remains deferred

- Define any managed backend security group mode before modifying worker ports owned by CAPO or an operator.
- Design Barbican identity, certificate rotation, rollback, and deletion before adding terminated HTTPS.
- Define multiple route selection, listener precedence, and shared backend ownership before expanding the current graph.
- Add per-Gateway infrastructure parameters only for concrete use cases with reviewed scope and merge rules.
- Keep other Octavia providers, resource adoption, Ingress, and proxy data planes outside this controller.

## Community and repository work

Seek independent reviews and reproducible Amphora reports without presenting repository-owner approval as community consensus.
Review governance, support, security contact paths, and release evidence before publishing artifacts for outside testing.
The criteria for maintainer growth and a future community home remain in [GOVERNANCE.md](../GOVERNANCE.md).

Pin GitHub Actions to reviewed commits and run race and envtest checks for the release candidate.
Verify branch protection and private vulnerability reporting in repository settings.
Files in this repository do not prove those settings are active.
