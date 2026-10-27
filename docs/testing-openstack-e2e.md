# Run the OpenStack controller E2E checks

This guide covers end-to-end checks for the current HTTP and NodePort implementation.
The checks use a real Kubernetes API server and an OpenStack Amphora data plane.
They do not run the Gateway API conformance suite and they do not establish general OpenStack compatibility.
See [compatibility evidence](providers/compatibility.md) for recorded coverage and its limits.

Follow the [test evidence policy](reports/README.md) when retaining artifacts and summarizing results in a PR.

## Before you start

Use a Kubernetes cluster reserved for this test.
The runner deletes a leader Pod, scales its controller Deployment to zero during recovery testing, and creates cluster-scoped RBAC with names unique to the run.
The `kubernetes.dedicatedForE2E` setting is an operator acknowledgement.
The runner cannot prove that no one else uses the cluster.

Prepare all of the following:

- Gateway API v1.6.1 Standard Channel CRDs on the cluster.
- The OpenStackGatewayClassConfig CRD from the revision under test, installed by the operator before starting the runner.
- A kubeconfig and explicit context that can create, inspect, update, and delete the run-scoped workloads, controller objects, and cluster-scoped RBAC.
  The test gets Namespaces, including `kube-system` when the cluster identity is pinned, and its ConfigMaps and Secrets.
  It lists GatewayClasses, manages its own class configurations, and reads configuration and Gateway API CRDs, Gateways, HTTPRoutes, EndpointSlices, ReplicaSets, Pods, and the leader Lease.
  The optional backend Node selector also requires permission to list Nodes.
  It deletes the current leader Pod, updates the controller Deployment, uses the `pods/proxy` subresource for metrics, and lists Gateways and HTTPRoutes for the audit.
  The optional lifecycle checks also update their GatewayClass references, class configuration, and backend Deployment.
- An OpenStack project with Amphora and enough quota for the Gateway graph and an optional Floating IP.
- A controller image built from the revision under test and an `agnhost` backend image, both available to the cluster and pinned by digest.
- An approved VIP subnet, member subnet, and explicit choice about using an external network.
- A path from Amphora to the selected Node `InternalIP` addresses and NodePorts.
- A path from the runner to the Kubernetes API, the OpenStack API endpoints used by the controller and audit credentials, and the resulting VIP or Floating IP.
- A redacted project-wide inventory recorded before the run.

Install the configuration CRD using the intended test context:

```sh
kubectl --kubeconfig /absolute/path/to/e2e.kubeconfig \
  --context openstack-e2e apply -k config/crd
kubectl --kubeconfig /absolute/path/to/e2e.kubeconfig \
  --context openstack-e2e wait --for=condition=Established \
  crd/openstackgatewayclassconfigs.gateway-api-openstack.github.io
```

The runner validates the installed CRD but does not manage it.
See the [class configuration reference](reference/class-configuration.md) for its fields and lifecycle.

## Choose where to run the suite

The suite sends HTTP requests from the process running the test to the address published by the Gateway.
The kubeconfig selects the Kubernetes API but does not route those requests through the cluster.
Run the suite on a host that can reach the Gateway address, or in a temporary Pod in the test cluster when the VIP is private.
For a Pod, make the source, tools, private configuration files, and artifact directory available at the paths specified in the configuration.
Keep the explicit workload cluster context and identity checks when running inside a Pod.

Check these paths separately:

| Path | Purpose |
| --- | --- |
| Runner to Kubernetes and OpenStack APIs | Create test objects, observe reconciliation, and audit ownership. |
| Controller to Kubernetes and OpenStack APIs | Reconcile the test Gateway graph. |
| Runner to Gateway VIP or Floating IP | Verify the HTTP response through Amphora. |
| Amphora to worker NodePort and backend Pod | Forward requests and perform health checks. |

Kubernetes API access does not imply access to worker NodePorts or a private VIP.
Run any NodePort diagnostic from the relevant network, then verify the complete path through the VIP.

## Prepare one configuration file

Copy the synthetic example to a private path:

```sh
install -d -m 700 .local/e2e-config
install -m 600 test/e2e/openstack.example.yaml \
  .local/e2e-config/openstack.yaml
```

Replace every placeholder before running the test.
The parser rejects unknown fields, and every path value in the file must be absolute.
The settings below describe the defaults and optional scenarios.
To pin a cluster, set `kubernetes.expectedAPIServer` to its HTTPS API endpoint and `kubernetes.expectedClusterUID` to the UID of its `kube-system` Namespace.
Supply both fields together.
They are required for the optional Local lifecycle checks and are checked before installation.

### Choose the OpenStack project mode

A disposable, dedicated OpenStack project is preferred and is expected for release evidence.
Keep this pair when the project is reserved for the run and empty of unrelated resources:

```yaml
openstack:
  projectMode: dedicated
  dedicatedForE2E: true
```

Use this pair when other engineers share the OpenStack project:

```yaml
openstack:
  projectMode: shared
  acceptProjectWideCredentialRisk: true
```

The two OpenStack acknowledgements are mutually exclusive.
They state what the operator has checked and do not change Keystone permissions.
Both modes still require the dedicated Kubernetes cluster acknowledgement.

Shared mode narrows the controller and audit to a unique logical identity, but the credential can still consume project quota, use Octavia and Neutron API capacity, allocate a Floating IP, and reach anything its roles permit.
Do not run load tests, quota exhaustion, OpenStack fault injection, security group changes, or destructive cloud recovery cases in a shared project.
Keep the run within the approved Gateway graph and use existing network and worker access rules.

### Set the project and network inputs

Set `openstack.expectedProjectID` to the exact project UUID approved for the run.
The runner authenticates the controller and audit configurations independently before it creates the controller, and both token projects must match this value.
The generated artifacts and public report must not contain the project ID.

Set `openstack.cloud` to the selected cloud entry and `openstack.region` to the exact region under test.
The runner fixes the provider to Amphora, the member mode to NodePort, and the Node address type to `InternalIP`.
`openstack.microversion` defaults to `2.5` and must be at least `2.29` when frontend groups are referenced.

Set `openstack.vipSubnetID` and `openstack.memberSubnetID` to the approved network IDs.
Set `openstack.externalNetworkID` to an exact external network ID when the test should allocate a Floating IP, or to the literal `none` when it should not.
The runner does not choose networks, reserve quota, or prove that the network path is safe.
The baseline run creates one OpenStackGatewayClassConfig using these exact subnet and external network IDs.
The test GatewayClass references this object through `spec.parametersRef`.

### Select network checks

Optional `openstack.frontendSecurityGroupIDs` and `openstack.backendSecurityGroupIDs` select existing groups for the test class.
Each list accepts up to 16 unique UUIDs and enables the corresponding `Referenced` mode.
The suite verifies frozen group IDs, the load balancer's VIP groups, backend port prerequisites, and ordinary HTTP traffic.
It never changes group rules or worker associations, including in shared project mode.
See the [security configuration requirements](reference/class-configuration.md#security-groups) before selecting these checks.

`scenarios.floatingIPRecovery: true` opts into one conditional detachment of the suite's existing Floating IP.
It requires `openstack.projectMode: dedicated`, an external network, and the exact cluster identity checks.
The scenario verifies the same Floating IP ID, address, and VIP association after recovery, then requires successful HTTP traffic and unchanged ownership inventory.
It does not create an additional address or load balancer.
Shared project mode rejects this scenario.

`scenarios.floatingIPDeletion: true` uses the same dedicated-project and cluster identity requirements.
It conditionally deletes only the run's owned, attached address and confirms its absence before observing a replacement.
The controller must recover through periodic observation without a Kubernetes spec edit, restore HTTP traffic, and preserve every other cloud resource ID.
The scenario replaces one address on the existing load balancer and does not retain two addresses at once.

These checks are disabled by default.
A passing report requires every selected check and the normal cleanup audit to pass.
Their availability is test coverage, not evidence of a completed OpenStack run.

### Set the credentials

`openstack.controllerCloudsYAML` must point to a file containing one self-contained cloud entry that uses an application credential.
TLS verification must remain enabled, so omit `verify` or set it to `true`.
The runner rejects profile inheritance, password or token authentication, `verify: false`, and references to additional CA, client certificate, or client key files.
It copies only the exact `clouds.yaml` bytes into the immutable controller Secret.
The `project_id` field may be omitted because the runner checks the project in the issued token.

The audit can use the controller credential in a dedicated evaluation project.
To use a separate credential, set `openstack.auditCloudsYAML` to its absolute path.
The audit file must follow the same one-entry, self-contained application credential and TLS verification rules because the runner authenticates its exact bytes before installation.
The OpenStack operator should grant this credential only the authentication and inventory permissions it needs.
The runner checks its project but cannot prove that its assigned roles are read-only.

### Pin the remaining inputs

Set `controller.domain` to a DNS domain you control.
Set `controller.image` to a complete image reference with a lowercase SHA-256 digest and set `controller.sourceRevision` to the full lowercase Git commit used to build it.
Set `backend.image` to a digest-pinned `agnhost` image.

Omit `runID` for normal runs so the runner generates a new DNS label.
An explicit `runID` must be between 8 and 32 characters and must not reuse a retained or previously recorded run.
Omit `artifacts` to write below `_artifacts/e2e/<run-id>`, or set `artifacts.root` to a non-root absolute directory.
The runner refuses to overwrite an existing run directory.

### Restrict a test backend to selected Nodes

The default backend uses two replicas with `externalTrafficPolicy: Cluster` and does not restrict Pod placement.
Set `backend.replicas` to `1` or `2` to match the selected worker capacity.
Nodes must satisfy the controller's [worker eligibility rules](reference/class-configuration.md#worker-nodeport-members).

Cluster mode can register every eligible worker even when backend Pods run on only a few Nodes.
For a shared project test restricted to selected workers, keep Local endpoint selection and a Node selector in the `backend` configuration:

```yaml
backend:
  externalTrafficPolicy: Local
  nodeSelector:
    e2e.example.test/backend: "true"
```

Replace the example label with one applied only to the intended worker Nodes, and keep those labels stable throughout the run.
The runner reads Node labels but does not change them.
Shared project mode requires Local traffic policy when a selector is set.
Dedicated mode also allows Cluster, but Pod placement alone does not restrict its cloud members.

The suite requires at least as many matching Ready, schedulable worker Nodes as `backend.replicas` before it creates the backend.
Required Pod anti-affinity places replicas on distinct selected Nodes.
Before creating the Gateway, the suite checks that EndpointSlices contain ready endpoints on those Nodes.
Local mode registers only Nodes with eligible local endpoints.
The operator must still verify the Amphora-to-NodePort path for the selected Nodes.

Validate each traffic policy separately when making a support claim.

### Enable Local lifecycle checks

Set `scenarios.localLifecycle: true` to add class configuration and backend transition checks.
This profile requires `externalNetworkID: none`, Local traffic policy, an explicit Node selector, and both cluster identity fields.
It supports either project mode with the corresponding acknowledgement.
Update these fields in the existing configuration:

```yaml
kubernetes:
  expectedAPIServer: https://api.example.test:6443
  expectedClusterUID: 33333333-3333-4333-8333-333333333333
backend:
  replicas: 1
  externalTrafficPolicy: Local
  nodeSelector:
    e2e.example.test/backend: "true"
scenarios:
  localLifecycle: true
```

These are additions to the configuration, not a standalone file.
Use `replicas: 2` when two eligible workers are available.
The test does not provision workers or change Node labels.

The class checks use two configurations with the same approved subnets and different health monitor delays.
They change the first configuration and its GatewayClass reference, then verify that the existing Gateway keeps its frozen settings and cloud resource identities, including after controller restart.
A fresh route observation must issue no OpenStack mutations.
After deleting the first Gateway graph and confirming an empty ownership inventory, the test creates a Gateway for the second class.
This uses two sequential load balancer lifecycles with at most one load balancer at a time.
Approve both lifecycles before running the profile.
It does not test simultaneous Gateways on different topologies.

| Backend replicas | Transition check |
| --- | --- |
| `1` | Run the class configuration and restart checks without a member transition. |
| `2` | Run the member transition checks described below. |

### Enable member transition checks

Set `scenarios.memberTransitions: true` to run the two-worker transition without the additional class lifecycles.
Use two backend replicas, a Node selector matching exactly two eligible workers, and both cluster identity fields.
Local supports either project mode.
Cluster requires a dedicated project, and the selected workers must be exactly the cluster's two eligible workers.
The runner checks this before creating a Gateway graph.

The scenario scales backend replicas from two to one and back.
Local must remove one member and later add its replacement while preserving the surviving member.
Cluster must retain both members throughout.
Both preserve every parent resource ID and perform the normal cleanup audit.
The runner does not create, delete, relabel, or change the readiness of Nodes.

During the transition, the runner samples HTTP with fresh connections at the configured polling interval and records failed samples without retrying them away.
This tests the transition that retains a healthy backend, not arbitrary Node replacement or a general drain policy.
The cloud mutation budget is two attempts for Local and zero for Cluster.
The request admission budget is the configured QPS multiplied by the observation duration plus burst, with rounding to a whole request.
This bounds the complete observation window rather than measuring every subsecond burst.
The leader and controller process must remain unchanged so counters remain comparable.

## Run the suite

Check the configuration and environment without creating resources:

```sh
make test-e2e-preflight E2E_CONFIG="$PWD/.local/e2e-config/openstack.yaml"
```

This runs the runner with `--preflight-only`.
It checks local inputs, the selected cluster identity, installed CRDs, run identity collisions, credential projects, and the empty ownership audit scope.
It does not install a controller or create Kubernetes or OpenStack resources.
Image availability, creation permissions, quota, and data plane connectivity still require separate preparation or a live run.

Run the live suite with one command:

```sh
make test-e2e E2E_CONFIG="$PWD/.local/e2e-config/openstack.yaml"
```

The target builds the ownership audit, compiles the runner and tagged E2E suite, installs a controller dedicated to the run, and executes the live checks.
The runner selects `TestOpenStackE2E` in `test/e2e/openstack_e2e_test.go`.
The YAML file is the source of truth.
Inherited `GATEWAY_OPENSTACK_*` and `OS_*` variables do not override the generated test environment.

These local targets do not contact Kubernetes or OpenStack:

```sh
make test-e2e-unit
make test-e2e-compile
```

Live E2E remains opt-in and is not part of `make verify`.

### Recurring dedicated validation

`make test-e2e-recurring E2E_CONFIG=/absolute/private/path/openstack.yaml` resolves the configuration for Cluster and Local and runs them sequentially with separate identities.
It overrides only the backend traffic policy for each profile.
It requires a dedicated project, `scenarios.memberTransitions: true`, two selected workers, both cluster identity fields, no fixed `runID`, and `scenarios.localLifecycle` disabled.
Set `artifacts.root` to an existing absolute directory outside the checkout, including after resolving symlinks.
The second profile starts only after the first passes its audit and removes its controller installation.
Any failure or cancellation stops the sequence.

The workflow in `.github/workflows/openstack-e2e.yml` can run weekly or through manual dispatch.
It accepts only `main` in the project repository and remains disabled unless the repository variable `OPENSTACK_E2E_ENABLED` is `true`.
Configure the protected `openstack-e2e` environment and one dedicated Linux runner carrying the `openstack-e2e` label.
That runner must have the approved API and VIP connectivity, tools, credentials, and immutable images described above.
Set `OPENSTACK_E2E_CONFIG` to a private configuration file outside the checkout on persistent storage.
Keep the configuration path stable so successive jobs use the same pending marker.

Before checkout, the workflow creates an exclusive `<config-path>.pending` directory.
It retains that marker and any private test logs if checkout, setup, testing, or cleanup fails.
A later job refuses to start while the marker exists, and raw logs are not uploaded to GitHub.
After failure, follow the recovery procedure below and confirm the owned graph and installation are absent before removing the marker.
Changing the configuration path or replacing the runner's persistent storage is not a cleanup procedure.
Successful jobs remove the marker and keep the per-run reports in `artifacts.root`.

## Installation and isolation

Before creating objects, the runner validates the configuration, cluster identity, installed CRDs, unused run identity, and the authenticated project of both credentials.
It requires a complete, empty ownership audit for the exact controller name and cluster ID.
These scope checks do not restrict the credential's OpenStack permissions.

The runner creates its controller installation with unique names and create-only writes.
It verifies the installation identities, immutable configuration and credential inputs, expected image digest, two Ready controller Pods, and a live leader Lease before testing the Gateway graph.
After successful graph finalization and an empty audit, it removes only installation objects whose UID and run annotation still match the recorded inventory.
Deletion uses UID preconditions.

## Foundation checks

The automated run covers the following checks:

1. Validate the pinned Gateway API bundle and the run-scoped controller installation.
2. Create an isolated NodePort backend.
3. Wait for the expected GatewayClass, Gateway, listener, and HTTPRoute status.
4. Require the active ownership audit to find both durable bindings and only matched OpenStack resources.
5. Send HTTP traffic through the address published in Gateway status.
6. Delete the current leader Pod and verify recovery with the same ownership inventory.
7. Stop and restore the controller Deployment and verify cold restart recovery.
8. Trigger a semantically equivalent HTTPRoute update and verify a fresh Octavia observation with no OpenStack mutation.
9. Delete HTTPRoute, Gateway, GatewayClass, and the workload Namespace in dependency order and wait for finalization.
10. Require the scoped ownership audit to return to its empty baseline.

The no-op check reads metrics from the current leader through the Kubernetes API.
It requires the OpenStack mutation counter to remain unchanged and the Octavia GET observation counter to increase while the leader and controller process remain stable.

The optional network scenarios inject only the approved owned-FIP detachment or deletion.
The runner does not inject quota, timeout, rate limiting, Octavia failure, blocked finalization, or referenced-security rule faults.
Keep those rows `Not run` unless a separate, reviewed mechanism exercises them in a dedicated project.
Unit, adapter, or envtest coverage does not turn an OpenStack E2E row into a pass.

## Audit and evidence limits

The ownership audit is required in both project modes.
It observes resources in the exact controller name and cluster ID scope before, during, and after the scenario.
It does not establish project-wide cleanup or authorize deletion.
See the [ownership audit contract](design/ownership-audit.md) for its discovery limits.

The runner writes `report.json` and `report.md` in a new artifact directory.
They record the tested artifact, configuration summary, check results, and aggregate audit and metrics evidence without cloud resource IDs or backend selector values.
Failures retain a fixed classification and the failed check, while raw API and audit errors remain excluded from the report.
Review them under the [publication policy](reports/README.md) before sharing.

The overall result is `Passed` only when every foundation check and selected lifecycle check passes, including active ownership evidence, finalization, and the post-test empty audit.
A failed assertion is `Failed`.
An optional check outside the run is `Skipped`.
A scenario that was not attempted or did not reach its assertion remains `Not run`.

## Failure and cleanup

If local validation, project authentication, or the pre-install audit fails, the runner stops without installing the controller.
If installation fails before it attempts to create the Deployment, it removes a partial installation only when every object still has the UID and run annotation it recorded.
Once it attempts to create the Deployment, it does not run automatic controller cleanup after a create, rollout, suite, audit, or cancellation failure.
The remaining controller objects and clouds Secret stay available for investigation and finalization.

Do not remove the retained controller, revoke its credential, force a finalizer, or delete an OpenStack resource by name.
Follow the [operator recovery guide](operator-recovery.md) and use the read-only audit until the owned graph is confirmed absent.
Do not reuse the run ID while any retained object or report remains.

After a successful run, compare a redacted project-wide final inventory with the starting inventory.
In shared mode, review concurrent differences with the project users and record any change that cannot be attributed.
The runner does not delete an OpenStack project, application credential, network, or unrelated resource.
