# ADR 0001: One writer for each Gateway graph

Status: Accepted

Date: 2026-08-18

Accepted: 2026-10-09

Owner: `jihyun-huh`

The repository owner approved this direction for Phase 3 implementation.
This record does not claim independent review or upstream consensus.

## Context

Gateway and HTTPRoute reconciliation change the same Octavia load balancer.
The previous implementation serialized separate `EnsureGateway` and `EnsureRoute` calls with a process-local coordinator.
Each call still observed part of the graph and built its own mutation plan.
Serialization alone could not make route detach intent durable or give either caller a complete view of the desired graph.

The replacement must preserve these invariants:

- Persist bindings before cloud creation.
- Persist status that reports replacement or rejection before cleanup.
- Keep route cleanup separate from deletion of its Gateway infrastructure.
- Validate the complete owned graph before whole-Gateway deletion.
- Perform at most one OpenStack mutation in each graph reconciliation.
- Rebuild desired state after restart without an in-memory fragment registry.
- Preserve the current route selection rule and exact HTTPRoute ownership identity.

This decision does not add multiple routes, change route precedence, or introduce shared pools.
[ADR 0002](0002-class-configuration.md) defines the separate class configuration API and worker eligibility contract.

## Decision

### One shared writer

The manager constructs one `graph.Writer` and passes it to both reconcilers.
The writer acquires the Gateway UID coordinator, collects current Kubernetes inputs through the resource packages, validates the resulting snapshot, and calls `cloud.Provider.ReconcileGatewayGraph`.
It is the only Kubernetes-facing cloud mutation boundary.
The provider also exposes the read-only `GetGateway` operation.

Gateway and HTTPRoute reconcilers retain acceptance, binding checkpoints, finalization, and their own status fields.
The writer does not patch Kubernetes objects.
Neither reconciler calls a separate Gateway or route mutation method.

The provider input contains a Gateway identity, one Gateway node, and the complete known route fragment set:

```go
type GatewayGraphSpec struct {
    Identity Identity
    Gateway  GatewayGraphNode
    Routes   []RouteGraphNode
}
```

The result contains a whole-graph outcome and separate Gateway and route outcomes.
`cloud.GraphError` identifies the affected component when it is known.

### Complete graph and dispositions

Each node has explicit authority:

| Disposition | Authority |
| --- | --- |
| `Ensure` | Reconcile the component against its complete desired model. |
| `Preserve` | Observe and validate the component without creating, updating, or deleting it. |
| `Delete` | Remove the exact owned component using a durable Kubernetes cleanup checkpoint. |

HTTPRoute finalization uses `Gateway=Preserve` and marks detaching route fragments `Delete`.
It cannot delete the load balancer, Floating IP, or listener.
Gateway finalization uses `Gateway=Delete` and removes the complete graph only after validating its ownership and dependencies.

A partial read, invalid binding, ambiguous lifecycle, or unaccounted route-scoped cloud resource blocks mutation.
A model error is not permission to delete an attached fragment.

### Route fragments and lifecycle

A fragment identifies one HTTPRoute and its desired pool, members, health monitor, L7 policies, and L7 rules.
The current HTTP profile permits at most one ensured route, alongside fragments that are being preserved or detached.
Gateway infrastructure belongs to the Gateway component.
Cached cloud IDs can assist observation but never establish ownership.
Fragments and their resource plans have deterministic ordering.

Each bound HTTPRoute records one controller-owned lifecycle annotation:

- `Attached` means the fragment participates in the desired graph.
- `Detaching` means the exact fragment must be removed before its binding can be cleared.

The HTTPRoute reconciler first makes rejection or replacement status durable when a ParentRef remains.
It does not invent a ParentStatus after that ParentRef has been removed.
It then validates the live cleanup decision and persists `Detaching` before calling the writer for deletion.
A deletion timestamp also requires this lifecycle checkpoint before cloud mutation.

`Detaching` remains in effect across restarts and later spec changes.
The controller clears the marker, binding, and finalizers only after the provider confirms that the fragment is absent.
A Gateway event cannot recreate a fragment while that marker remains.

Missing or invalid lifecycle markers on bound fragments are rejected.
The repository owner explicitly waived Phase 2 migration compatibility for this pre-alpha implementation.
There is no implicit `Attached` fallback, compatibility release, staged rollout, or supported downgrade path to the previous implementation with separate writers.

### Live graph construction

The writer acquires the Gateway UID lock before collecting its final snapshot.
The Gateway source checks the current object, binding, controller selection, and supported Gateway API bundle for provisioning.
It uses the topology already frozen in a bound Gateway rather than resolving a new class template.
The frozen topology survives replacement of the cloud graph for the same Gateway UID.

The route source lists the entire Gateway namespace through the uncached API reader.
It includes bound fragments even after their ParentRef is removed, and unbound candidates whose presence can change route selection.
Cached indexes are useful for event mapping but do not prove that this inventory is complete.
A future cross-namespace profile needs a new completeness strategy.

Before the provider call, the source reads the inventory again and compares route UIDs and resource versions.
It also applies the required per-object checks for generation, deletion state, finalizers, binding, lifecycle, controller ownership, and installed API version.
The reads remain separate Kubernetes operations.
The writer rejects changes detected between them.

The writer stores no fragment registry or mutation plan between calls.
The coordinator serializes work within one process, and leader election prevents multiple active mutating replicas.
Restart and leader change require fresh observation, not lock recovery.

A missing or replaced Gateway blocks normal `Ensure` work but does not erase a bound HTTPRoute's cleanup authority.
A complete binding and durable `Detaching` marker permit `Gateway=Preserve` with `Route=Delete` for the recorded Gateway UID.
That path never creates infrastructure or deletes the Gateway component.

### Provider observation and planning

The OpenStack adapter validates the entire desired input, observes the relevant cloud graph, and validates ownership before executing a mutation.
For an active load balancer, observation accounts for every route descendant rather than only the triggering HTTPRoute.
An unexpected route-scoped child cannot be adopted or garbage collected.
While Octavia reports `PENDING_*`, the adapter returns progress without mutation and observes again on a later call.

The adapter builds one ordered plan:

1. Reconcile Gateway prerequisites.
2. Remove detaching fragments.
3. Reconcile the attached fragment.
4. Restore listener administrative state.
5. Restore load balancer administrative state.

It executes only the first step.
Whole-Gateway deletion uses validated reverse dependency order.
The graph operation does not delegate to sequential `EnsureGateway` and `EnsureRoute` calls.
A converged second pass makes no mutating OpenStack call.

The adapter discovers Floating IPs independently of the load balancer by listing the authenticated project and checking the complete Gateway identity.
This finds a detached owned address even when the load balancer still exists and detects foreign addresses on its VIP port.
Normal provisioning stops when an owned address is detached or its load balancer is absent, preventing duplicate allocation.
It does not reconnect the address automatically.
Whole-Gateway deletion may remove a uniquely owned, fully detached address after validating any remaining load balancer graph.
Duplicate candidates, inconsistent identity, and unexpected port, fixed-address, or router associations block mutation.
Route-only cleanup preserves the Gateway's Floating IP.

### Component results and status

The whole-call outcome drives scheduling while component outcomes describe the Gateway and each route.
A route error discovered during a Gateway-triggered write is not reported as a Gateway provisioning failure.
A pending route change does not clear an otherwise valid Gateway `Programmed` condition.

Reconcilers translate the results into their existing status contracts and preserve other controllers' HTTPRoute parent entries.
Component results do not claim backend health or uninterrupted traffic.
Replacing every member still uses ordered deletion before creation and can leave a pool empty between reconciliations.

## Consequences

Gateway and HTTPRoute events use the same observation and mutation boundary.
The complete desired graph is reconstructible after restart, and detach intent remains durable.
The cost is a complete live route inventory and an additional metadata checkpoint before route deletion.

Pure desired-model and diff functions remain separate from Kubernetes reads and provider I/O.
Event mappers may reuse dependency reads within one event, but those observations never authorize cloud mutation.
Writer metrics separate lock wait, Gateway input, route input, validation, and provider duration.
The provider duration includes local waits during one call and does not measure asynchronous Octavia convergence.

## Alternatives considered

### Keep separate mutations behind a coordinator

This prevents overlap but retains independent desired snapshots and mutation plans.

### Wrap the existing provider methods

Calling Gateway and route methods in sequence retains separate observations and can perform more than one mutation.

### Store fragments in memory or a separate CRD

An in-memory registry loses cleanup intent after restart.
A separate fragment CRD adds an unnecessary API and lifecycle for the current HTTP profile.
The durable HTTPRoute binding supplies the required state.

### Make only Gateway reconciliation drive progress

This makes HTTPRoute cleanup and component status depend on another controller's scheduling.
The shared writer permits either reconciler to drive progress while retaining one mutation boundary.

## Validation and remaining evidence

Local controller and adapter tests cover complete input validation, live inventory changes, binding checkpoints, lock cancellation, route-only cleanup, component outcomes, and reconstruction with a new writer or provider.
Adapter tests also cover deterministic ordering, a converged no-op, pending state, lost responses, unknown descendants, administrative repair, reverse deletion, Floating IP recovery, and intermediate member replacement states.
Race tests and envtest exercise the Kubernetes lifecycle and serialization boundaries.

The previous baseline OpenStack E2E run predates this writer.
Local tests and ADR acceptance do not establish compatibility or Gateway API conformance.
The implemented writer still needs fresh live OpenStack evidence and the planned conformance gap analysis under the [test evidence policy](../../reports/README.md).
Public image publication and scheduled live validation remain release work described in the [roadmap](../../../ROADMAP.md).
