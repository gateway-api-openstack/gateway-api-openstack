# ADR 0002: Class configuration and development identity

Status: Accepted

The development API identity in this record is superseded by [ADR 0003](0003-project-organization.md).
The class configuration, topology binding, and worker selection decisions remain in effect.

Date: 2026-10-09

Owner: `jihyun-huh`

Approval: The repository owner directed this pre-alpha implementation, selected the development API group, and explicitly excluded Phase 2 migration compatibility from its scope.
No public review issue or community consensus is claimed.

## Context

Deployment-wide subnet and health monitor flags give every Gateway the same infrastructure settings.
Phase 3 needs typed GatewayClass configuration while keeping credentials and cloud ownership within one installation's authenticated project.
The controller also needs a defined worker selection rule before replacing the label workaround used in the earlier development test.

This repository is maintained by one developer and has no supported release or upgrade contract.
The owner chose to implement Phase 3 directly, without a compatibility release for Phase 2 bindings or flags.
The decision remains bounded by the existing Amphora, NodePort, and one-listener HTTP profile.
It does not authorize backend security group management, resource adoption, or a broader routing model.

## Decision

### Development API identity

Use `gateway.jihyun-huh.github.io` as the development API group and `gateway.jihyun-huh.github.io/gateway-api-openstack` as the default controller name.
This is the identity approved by the repository owner for development.
It does not settle a permanent release domain, artifact registry, project transfer, or support commitment.
The configured controller name remains an exact ownership identifier and must match each managed GatewayClass.

### One typed class configuration

Add the cluster-scoped OpenStackGatewayClassConfig kind at `gateway.jihyun-huh.github.io/v1alpha1`.
Require every managed GatewayClass to reference it through `spec.parametersRef`, with the exact group and kind and no namespace.
Missing or unsupported references are invalid parameters.
Do not retain infrastructure flags as a fallback and do not add per-Gateway configuration overrides.

The class API selects existing VIP and member subnets, an optional external network for allocating a new Floating IP, a Node address type, bounded HTTP health monitor settings, and optional Amphora flavor and availability zone placement.
References use one UUID, one exact name, or an AND tag selector that resolves to exactly one resource.
The [configuration reference](../../reference/class-configuration.md) defines the fields, defaults, limits, and minimum microversions.

Credentials, authenticated project, cluster identity, region, controller identity, and client request limits remain deployment concerns.
The class cannot change their scope.
The provider is always Amphora and the member mode is always NodePort.
Reading a selected network or subnet does not transfer ownership to the controller.

### Resolve before binding, then retain the result

Resolve and validate class parameters with fresh, bounded, read-only cloud requests before a new Gateway acquires its binding.
Verify project ownership of VIP and member subnets, selected network properties, advertised API support, and the Amphora provider behind optional placement profiles.
Do not infer mutation permissions or connectivity from those reads and do not issue trial writes.
A denied optional capability read remains a validation prerequisite that has not succeeded.

Persist the resolved configuration and parameter object name, UID, and generation in the Gateway binding before creating cloud resources.
Anchor the serialized configuration with a digest in a controller finalizer to detect incomplete or accidental metadata edits.
This is an integrity check, not a security boundary against a principal allowed to edit finalizers.

The configuration is fixed for the lifetime of that Gateway UID.
Parameter changes, parameter deletion, listener replacement, and cleanup after an invalid spec do not replace the stored configuration.
A managed Gateway retains it through graph replacement and uses it when the spec becomes valid again.
Finalization uses the binding without resolving live parameters.
A new configuration requires deletion to finish and creation of a new Gateway UID.

### Worker selection

Exclude every Node carrying `node-role.kubernetes.io/control-plane` or `node-role.kubernetes.io/master`, including mixed-role Nodes.
Do not require a worker role label and do not offer a control plane opt-in.
All remaining members must be ready, schedulable, not deleting, and have a valid address of the class's selected type.

Apply `node.kubernetes.io/exclude-from-external-load-balancers` using the current Kubernetes Service controller boolean semantics.
An absent label allows eligibility, an explicit false value allows eligibility, and empty, true, or invalid values exclude the Node.
This label cannot override the control plane exclusion.
Apply the same rules to both NodePort traffic policies, with `Local` additionally requiring a ready endpoint on the worker.

### Maintain a small API surface

Maintain the CRD schema and DeepCopy methods by hand alongside the Go API types.
Use structural schema, defaulting, CEL, and envtest checks to keep them consistent.
Do not introduce a Kubebuilder scaffold, a webhook, or a generation workflow for this API.
The configuration object has no status subresource.
Its validation result belongs to the referencing GatewayClass.

## Consequences

Different classes can select distinct infrastructure within one authenticated project without duplicating controller deployments.
Application teams cannot move a bound Gateway by editing mutable class defaults.
Deleting a parameter object can block new Gateways while existing bindings retain enough information to reconcile and finalize.

Topology changes require recreating the Gateway, which is a deliberate operator cost at this stage.
Optional flavor and availability zone validation can require profile reads that a cloud does not grant to ordinary project credentials.
A successful validation still cannot establish data plane reachability, quota, or permission for later creates and deletes.

The configuration CRD is cluster-scoped and is included in the base installation.
Removing that CRD deletes its configuration instances throughout the cluster and affects every installation using the API.
Uninstall procedures must account for those shared resources.

## Alternatives considered

### Keep deployment-wide topology flags

This keeps configuration simple for one class but prevents classes from selecting independent infrastructure.
Retaining both flags and parameters would also require precedence and compatibility rules that the owner excluded from this development milestone.

### Resolve selectors on every Gateway reconciliation

This lets renamed or retagged resources move an existing Gateway's desired topology without a new Kubernetes identity.
It also makes cleanup depend on mutable configuration.
A durable resolved binding gives the controller a stable configuration for recovery.

### Add per-Gateway overrides or managed backend security

Neither is needed for the current HTTP profile.
Overrides require a separate policy and merge contract, and attaching security groups changes worker ports owned by another component.
Those features remain outside this decision.

## Safety and recovery

Class validation performs reads only.
Cloud mutation still requires the complete immutable project, cluster, controller, Gateway, route, and resource-role identity checks described in [ADR 0001](0001-gateway-graph-writer.md).
Names, tags used as selectors, and stored IDs do not prove ownership of a load balancer graph.

A missing, partial, or changed topology binding stops mutation rather than reconstructing authority from current class defaults.
A missing parameter object does not remove an existing graph or prevent its finalization.
Keep the configuration CRD, original controller identity, and original project access available throughout cleanup.
The [operator recovery guide](../../operator-recovery.md) applies when ownership or binding integrity cannot be proved.

## API and upgrade impact

The owner waived Phase 2 migration compatibility for this pre-alpha change.
Old topology flags and implicit defaults are removed, and earlier bindings are not converted automatically.
Do not roll this implementation over development resources that still need an earlier controller's cleanup behavior.
Finalize those resources with the original version and identity before recreating them with the new API.

Changing the API group, default controller identity, or durable binding contract requires another decision.
The development workflow may clean up and recreate resources instead of providing an automatic migration.
There is no adoption path, no automatic identity rewrite, and no promise that the development domain will be the final release domain.

This API does not expand the implemented Gateway API routing profile or justify entries in `GatewayClass.status.supportedFeatures`.
API acceptance and local test success do not establish conformance or OpenStack compatibility.

## Test and evidence

The implementation has unit and adapter tests for selector ambiguity, project scope, read-only capability discovery, optional profile validation, frozen bindings, worker membership, and redacted provider failures.
Schema tests and envtest exercise defaults and validation through the Kubernetes API server.
Controller tests cover binding before cloud creation, parameter changes and deletion, retention through graph replacement, and finalization from the stored configuration.
Local verification, race tests, lint, build checks, and envtest passed before this record was written.

The earlier Phase 2 OpenStack E2E result does not validate the Phase 3 API or graph writer.
A new live run and the pinned Gateway API conformance gap analysis remain required evidence under the roadmap.
No public release or compatibility claim follows from accepting this ADR.

## Follow-up work

- Validate the Phase 3 configuration and graph writer in the opt-in OpenStack E2E suite.
- Run the pinned HTTP conformance profile and publish the resulting gap analysis.
- Decide the release identity, artifact registry, and supported upgrade policy before publishing artifacts for outside use.
- Define any per-Gateway overrides or shared backend security group mutation in separate ADRs.
