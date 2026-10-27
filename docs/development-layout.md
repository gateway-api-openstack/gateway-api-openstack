# Development package and file layout

Status: current guidance.

Split code by ownership, lifecycle, and API boundaries.
A long file can reveal mixed responsibilities, but line count alone does not justify another package.
Each package adds imports, exported contracts, and possible dependency cycles, so its responsibility should be clear.

## Upstream patterns

Kubernetes splits controller files by behavior and keeps smaller controllers together when their responsibilities fit.
Its [coding conventions](https://github.com/kubernetes/community/blob/main/contributors/guide/coding-conventions.md#directory-and-file-conventions) discourage package sprawl and broad utility packages.

Cluster API's [Cluster reconciler](https://github.com/kubernetes-sigs/cluster-api/blob/main/core/reconcilers/cluster/cluster_controller.go) uses a scope local to each request, named lifecycle phases, and deferred patching.
Its [dependency mappers](https://github.com/kubernetes-sigs/cluster-api/blob/main/util/util.go) list affected objects within a defined scope and return reconcile requests.
Keep validation and lifecycle work in reconciliation rather than turning a mapper into another control loop.

CAPO separates [OpenStack service packages](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/tree/78ef68d5853a85e48e6f6bf69a353b3a1d073526/pkg/cloud/services) from [reconciliation scopes](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/blob/main/pkg/scope/scope.go).
The [OpenStackCluster reconciler](https://github.com/kubernetes-sigs/cluster-api-provider-openstack/blob/main/controllers/openstackcluster_controller.go) keeps lifecycle decisions at the controller boundary.
This repository follows those responsibilities while keeping Kubernetes types outside its OpenStack adapter.

cloud-provider-openstack's Octavia Ingress Controller also separates its [controller](https://github.com/kubernetes/cloud-provider-openstack/blob/ff04631653b624a75c811a0836f2ba4a0395d04e/pkg/ingress/controller/controller.go) from an [OpenStack adapter](https://github.com/kubernetes/cloud-provider-openstack/tree/ff04631653b624a75c811a0836f2ba4a0395d04e/pkg/ingress/controller/openstack).
Use these projects to guide boundaries, not to impose a file-size limit or duplicate their abstractions.

## Public configuration API

[ADR 0002](design/adr/0002-class-configuration.md) defines the project-owned OpenStackGatewayClassConfig API.
Its implementation is small and maintained directly:

```text
api/v1alpha1/
├── groupversion_info.go
├── types.go
├── deepcopy.go
└── schema_test.go
config/crd/
├── gateway-api-openstack.github.io_openstackgatewayclassconfigs.yaml
└── kustomization.yaml
```

Go types, DeepCopy methods, OpenAPI and CEL validation, and the CRD are maintained by hand.
Schema tests and envtest check the public validation contract.
There is no Kubebuilder `PROJECT`, webhook scaffold, or generation workflow.
Keep these files consistent with the [class configuration reference](reference/class-configuration.md).

## Controller layout

Each core resource has its own reconciler package.
The following tree omits tests and some focused files:

```text
internal/controller/
├── config.go
├── binding.go
├── topology.go
├── route_lifecycle.go
├── nodes.go
├── gateway_api_version.go
├── indexes.go
├── predicates.go
├── provider_outcomes.go
├── classconfig/
│   ├── validation.go
│   └── resolve.go
├── gatewayclass/
│   ├── gatewayclass_controller.go
│   ├── gatewayclass_scope.go
│   └── gatewayclass_watch.go
├── gateway/
│   ├── gateway_controller.go
│   ├── gateway_validation.go
│   ├── gateway_binding.go
│   ├── gateway_graph.go
│   ├── gateway_graph_source.go
│   ├── gateway_snapshot.go
│   ├── gateway_cleanup.go
│   ├── gateway_status.go
│   ├── gateway_scope.go
│   └── gateway_watch.go
├── httproute/
│   ├── httproute_controller.go
│   ├── httproute_parent.go
│   ├── httproute_selection.go
│   ├── httproute_model.go
│   ├── httproute_backend.go
│   ├── httproute_backend_model.go
│   ├── httproute_binding.go
│   ├── httproute_graph.go
│   ├── httproute_graph_source.go
│   ├── httproute_snapshot.go
│   ├── httproute_cleanup.go
│   ├── httproute_status.go
│   ├── httproute_scope.go
│   ├── httproute_watch.go
│   └── httproute_event_inputs.go
├── graph/
│   ├── coordinator.go
│   ├── writer.go
│   └── metrics.go
└── ownershipaudit/
    └── collector.go
```

The root `controller` package owns shared Kubernetes-facing contracts such as deployment configuration, durable bindings, frozen topology, API version observation, indexes, and outcome policy.
It does not import resource reconcilers.
`classconfig` validates the public parameter API and resolves it through the provider-neutral topology interface.

`graph.Writer` implements [ADR 0001](design/adr/0001-gateway-graph-writer.md).
It coordinates by Gateway UID, calls Gateway and route source interfaces for fresh models and validation, and invokes the single cloud graph operation.
It does not import concrete reconciler packages, store fragments between calls, or patch status.
Gateway and HTTPRoute packages implement the sources and retain their lifecycle and status responsibilities.
HTTPRoute also reuses the exported Gateway listener validation entry point.

The command wires these dependencies together:

```text
cmd/openstack-gateway-controller
  |--> controller/gatewayclass --> controller, classconfig, cloud
  |--> controller/gateway -----> controller, classconfig, graph, cloud
  |--> controller/httproute ---> controller, gateway, graph, cloud
  `--> cloud/openstack --------> cloud

controller/graph -------------> cloud and Gateway API types
controller/ownershipaudit ----> controller, audit, cloud

cmd/openstack-gateway-diagnostics
  |--> controller/classconfig, diagnostics
  `--> cloud/openstack/diagnostics --> diagnostics, cloud
```

Use the following file boundaries inside a reconciler package:

- `*_controller.go` contains immutable dependencies, RBAC markers, `Reconcile`, and lifecycle dispatch.
- `*_validation.go` and pure model files validate inputs and calculate desired values.
- Backend input readers remain separate from member calculation.
- `*_binding.go` persists identity metadata and finalizers before cloud creation.
- `*_graph.go` drives the shared writer and interprets component results.
- `*_graph_source.go` supplies complete current inputs, and `*_snapshot.go` implements live validation checks.
- `*_cleanup.go` owns detach and finalization phases.
- `*_status.go` calculates conditions and ParentStatus entries.
- `*_scope.go` contains request-local state and deferred patching.
- `*_watch.go` maps events through indexes, with request-local read reuse in focused event input files.

A mapper's temporary read reuse is not a cache that can authorize mutation.
The writer still reads a complete live route inventory and validates ownership before calling OpenStack.
Keep pure calculations separately benchmarkable from Kubernetes reads, lock waits, and provider calls.
Do not add general `common`, `helpers`, or `util` packages.

## Provider-neutral boundaries

`internal/cloud` contains the desired graph, component results, topology values, and small provider interfaces.
It imports neither Kubernetes nor Gophercloud.
`internal/audit` likewise contains provider-neutral inventory and report types.
Its findings remain advisory and cannot authorize graph mutation.
`internal/diagnostics` defines the environment report and redacted outcomes without Kubernetes or Gophercloud dependencies.

## OpenStack adapter layout

The adapter follows service boundaries while keeping complete graph validation and planning together:

```text
internal/cloud/openstack/
├── provider.go
├── topology.go
├── compatibility.go
├── wait.go
├── clients/
│   ├── client.go
│   └── request_observer.go
├── observability/
│   └── metrics.go
├── apierrors/
│   └── classify.go
├── identity/
│   └── metadata.go
├── octavia/
│   ├── service.go
│   ├── loadbalancer.go
│   ├── listener.go
│   ├── pool.go
│   ├── monitor.go
│   ├── l7policy.go
│   └── wait.go
├── neutron/
│   ├── service.go
│   └── floatingip.go
├── topology/
│   ├── resolver.go
│   ├── neutron.go
│   └── capabilities.go
├── graph/
│   ├── provider.go
│   ├── graph.go
│   ├── graph_plan.go
│   ├── gateway.go
│   ├── route.go
│   ├── route_graph.go
│   ├── gateway_deletion_plan.go
│   ├── delete.go
│   └── loadbalancer_status.go
├── inventory/
│   ├── scanner.go
│   └── classification.go
└── diagnostics/
    └── scanner.go
```

`clients` creates shared authenticated clients, applies request limits, and emits bounded observations.
`observability` records those observations as process-local Prometheus metrics.
`apierrors` classifies SDK and HTTP failures, and `identity` encodes immutable ownership metadata.

`octavia` and `neutron` contain service operations.
`topology` performs read-only reference resolution and capability checks through those service clients.
It returns cloud values, not SDK objects or Kubernetes conditions.

`graph` validates the complete desired model, observes cloud ownership, builds one ordered plan, and executes at most one mutation.
Its resource files contain focused observation, diff, and execution logic used by that operation.
`inventory` exposes only read operations for the ownership audit.
`diagnostics` reads endpoint capabilities, topology, permissions, and quota metadata and returns provider-neutral diagnostics.
It never changes resources or reads Barbican secret payloads.
The `openstack-gateway-diagnostics` command supplies provider-neutral values read from Kubernetes, wires the scanner directly, and renders the report.

The root `openstack` package is the facade used by commands and probes.
It delegates to graph reconciliation, topology resolution, and read-only inventory without leaking SDK types into callers.
Child packages never import the facade, and Kubernetes types do not enter any OpenStack package.

The [architecture](design/architecture.md#kubernetes-events-and-measurements) defines the boundaries of writer and OpenStack request metrics.

## Test layout

Keep unit tests and benchmarks beside the behavior they exercise.
HTTPRoute event tests and benchmarks live in `internal/controller/httproute`.
Keep mapper read measurements separate from tests that follow events through reconciliation.

The tagged tests in `internal/controller/envtest*_test.go` use a real API server and cache for schema validation, binding checkpoints, conflicts, and class configuration isolation.
Cloud behavior remains fake.
The [compatibility record](providers/compatibility.md#evidence-matrix) describes what these checks establish.

`test/e2e/` contains the opt-in [OpenStack E2E runner](testing-openstack-e2e.md).
Ordinary verification does not enable live execution.
The [conformance guide](testing-conformance.md) describes the planned full-profile suite and its prerequisites.
