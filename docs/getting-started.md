# Getting started with the current controller

The controller is pre-alpha.
No supported environment profile or release image is available.
See the [compatibility record](providers/compatibility.md) for development evidence and the [roadmap](../ROADMAP.md) for remaining work.

Use these base manifests only in a disposable test project.
Keep cloud-provider-openstack responsible for Services of type `LoadBalancer`.
This controller creates separate resources only for its Gateway API objects.

For automated validation, use the [OpenStack E2E runner](testing-openstack-e2e.md), which creates a separate test installation in a dedicated or shared project.
The [conformance plan](testing-conformance.md) describes the separate Gateway API requirements and current gaps.

## Prerequisites

- A Kubernetes cluster with the Gateway API Standard Channel v1.6.1 CRDs.
- An OpenStack project with Octavia and Neutron access.
  The controller accepts only Amphora.
  Support claims are limited to environment profiles with published compatibility evidence.
- A Keystone application credential with only the permissions that the controller needs, stored in a named `clouds.yaml` entry.
- A VIP subnet and a member subnet from which Amphora can reach the selected Kubernetes Node addresses and allocated NodePorts.
- Security groups and routing that allow Amphora health checks and HTTP traffic to those Node addresses and NodePorts.
- Optionally, an external network for allocating a Floating IP.
  Without one, Gateway status reports the Octavia VIP, which may be reachable only from private networks.
- A controller container image built from the source revision being deployed.
  The repository does not currently publish a supported image.

Each class must select a VIP subnet and a member subnet through an OpenStackGatewayClassConfig.
`InternalIP` is the default Node address type.
Select `ExternalIP` only when those addresses belong to the configured member network and are reachable from Amphora.
Only eligible worker Nodes become members.
See the [class configuration reference](reference/class-configuration.md#worker-nodeport-members) for control plane exclusions and traffic policy behavior.

The controller does not create networks, subnets, routers, or backend security groups and does not attach security groups to worker ports.
Establish that connectivity before creating a Gateway.

Install the pinned Standard Channel CRDs if the cluster does not already have them:

```sh
kubectl apply -f \
  https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml
```

Startup requires GatewayClass, Gateway, and HTTPRoute to be available.
The controller also checks that installed Gateway API CRDs have the pinned bundle annotation.
A missing or mixed annotation produces `SupportedVersion=False` and `Accepted=False` with reason `UnsupportedVersion` on GatewayClass and blocks provisioning and repair.
Existing graphs remain in place and can still be finalized, as described in the [API version contract](design/architecture.md#installed-gateway-api-version).

## Prepare OpenStack credentials

Create a local `clouds.yaml` containing an application credential.
Do not add this file to the repository.

```yaml
clouds:
  openstack:
    auth:
      auth_url: https://identity.example/v3
      application_credential_id: replace-me
      application_credential_secret: replace-me
    auth_type: v3applicationcredential
    region_name: RegionOne
    interface: public
    identity_api_version: 3
```

Create the controller namespace, then store the file as a Secret.
The Secret name and key match the Deployment volume in `config/manager/deployment.yaml`.

```sh
kubectl apply -f config/default/namespace.yaml
kubectl -n openstack-gateway-system create secret generic openstack-clouds \
  --from-file=clouds.yaml=/absolute/path/to/clouds.yaml
```

The Secret is mounted read-only and credentials are not copied into ConfigMaps, Gateway API status, or examples.

## Configure and install

Edit `config/manager/controller-config.yaml` before applying it:

- Keep the default controller name, or choose an installation identity before creating managed resources.
- Set the cluster ID to a value that is unique to this cluster, and do not change it while managed resources exist.
- Set `OS_CLOUD` to the entry in the mounted `clouds.yaml`.
- Set `OS_REGION_NAME` only when it should override `region_name` in that file.

The default controller name is `gateway-api-openstack.github.io/gateway-api-openstack`.
The same value must appear in every managed `GatewayClass.spec.controllerName`, including `examples/basic.yaml`.
The API group is `gateway-api-openstack.github.io`.
An existing development installation must complete cleanup with its original controller before changing identities, as described in the [compatibility guidance](reference/class-configuration.md#development-compatibility).

Configure VIP and member placement in the OpenStackGatewayClassConfig in `examples/basic.yaml`.
Replace its synthetic subnet IDs with existing resources in the controller project.
Keep `floatingIP.mode: None` to publish the VIP, or use `Allocate` with an explicit external network reference.
The [class configuration reference](reference/class-configuration.md) covers selectors, health monitor defaults, optional placement, and required reads.
The controller does not use deployment topology flags when class parameters are missing.

Build and push an image from the exact source revision you are deploying, then edit the image override in `config/default/kustomization.yaml`.
The image reference in the repository uses `example.invalid` and is intentionally unusable, so it cannot be mistaken for a published release.

```sh
make container-build \
  IMAGE=registry.example/openstack-gateway-controller:v0.3.0-dev \
  VERSION=v0.3.0-dev
docker push registry.example/openstack-gateway-controller:v0.3.0-dev
```

Set `CONTAINER_TOOL=podman` on the `make` command when Podman is preferred.

The base installation includes the cluster-scoped OpenStackGatewayClassConfig CRD.
Inspect its effect on any other installation using this API before applying or removing the base manifests.
Render and inspect the resources, then install them:

```sh
kubectl kustomize config/default
kubectl apply -k config/default
kubectl -n openstack-gateway-system rollout status \
  deployment/openstack-gateway-controller
```

The Deployment starts two replicas with leader election.
It exposes health and readiness probes on port 8081.
Port 8080 exposes controller-runtime metrics and bounded OpenStack request, duration, mutation attempt, and in-flight metrics inside each Pod.
These OpenStack metrics are process-local and reset when the controller process restarts.
The base manifests do not expose metrics through a Service.

## Deployment settings

The ConfigMap contains process identity and operational settings, with no secrets.
Infrastructure choices belong to the class API.
The ConfigMap uses the environment variables accepted by the controller binary.

| Setting | Environment variable | Current behavior |
| --- | --- | --- |
| Controller identity | `GATEWAY_OPENSTACK_CONTROLLER_NAME` | Defaults to `gateway-api-openstack.github.io/gateway-api-openstack`. Must exactly match GatewayClass. |
| Cluster identity | `GATEWAY_OPENSTACK_CLUSTER_ID` | Required and stable. |
| OpenStack resync | `GATEWAY_OPENSTACK_RESYNC_INTERVAL` | Base interval for observing converged resources again. Defaults to `10m` |
| OpenStack request rate | `GATEWAY_OPENSTACK_API_QPS` | Average request limit shared by Keystone, Octavia, and Neutron. Defaults to `10` per second |
| OpenStack request burst | `GATEWAY_OPENSTACK_API_BURST` | Short request burst allowed for the process. Defaults to `20` |
| Cloud name | `OS_CLOUD` | Named entry in `clouds.yaml` |
| Cloud file | `OS_CLIENT_CONFIG_FILE` | Mounted `/etc/openstack/clouds.yaml` |
| Region | `OS_REGION_NAME` | Optional override |

The controller uses a 10 minute base interval by default, with each observation scheduled between about 8 and 12 minutes.
Stable jitter based on the resource UID prevents large groups of resources from reaching OpenStack at the same time.
Set the environment variable or use `--openstack-resync-interval` to change the base interval.
The command line flag takes precedence, and the value must be greater than zero.

Every OpenStack service client in the process shares one request budget.
This also covers authentication and token renewal, so increasing controller worker counts cannot bypass the limit by using another service client.
Both request settings must be greater than zero.
The equivalent command line flags are `--openstack-api-qps` and `--openstack-api-burst`.

The base Deployment also sets these binary flags: leader election enabled, metrics on `:8080`, probes on `:8081`, and Octavia microversion `2.5`.
Optional flavor and availability zone placement require higher microversions and additional profile reads, as described in the [configuration reference](reference/class-configuration.md#validation-and-permissions).
The binary additionally supports `--openstack-operation-timeout` and `--openstack-poll-interval`.
The manifests do not override these two flags, so the binary uses defaults of 10 minutes and 2 seconds.
The timeout limits each provider reconciliation call, including all OpenStack requests made by that call.
After starting an asynchronous Octavia change, the controller returns and observes it again after the poll interval instead of waiting for it to finish in the same reconciliation.

## Apply the basic example

Confirm the controller name and class subnet references in `examples/basic.yaml`, then apply it:

```sh
kubectl apply -f examples/basic.yaml
kubectl get openstackgatewayclassconfig openstack -o yaml
kubectl get gatewayclass openstack -o yaml
kubectl -n gateway-api-openstack-demo get gateway edge -o yaml
kubectl -n gateway-api-openstack-demo get httproute basic -o yaml
```

The example intentionally uses one Gateway, one HTTP listener, one HTTPRoute, one rule, one PathPrefix match, and one NodePort Service backend.
First check that GatewayClass reports `Accepted=True`.
This verifies the class inputs and read-only cloud checks, not network connectivity or provisioning permissions.
Then wait until the Gateway's `Accepted` and `Programmed` conditions are `True`.
In the HTTPRoute parent status, wait for `Accepted`, `ResolvedRefs`, and the `<controller-domain>/Programmed` condition defined by this controller to become `True`.
The address is published at `Gateway.status.addresses[0].value`.

```sh
kubectl -n gateway-api-openstack-demo get gateway edge \
  -o jsonpath='{.status.addresses[0].value}{"\n"}'
```

Send an HTTP request to that address from a network that can reach it.
A VIP without a Floating IP may not be reachable from the public Internet.

## Current implementation limitations

- HTTP is the only listener protocol.
  TLS is not supported.
- Each Gateway has exactly one listener and one selected HTTPRoute in the same namespace.
- Each HTTPRoute has one rule, at most one exact hostname, at most one Exact or PathPrefix match, and one Service backend in the same namespace.
- Backend Services must be `type: NodePort`.
  Both `externalTrafficPolicy: Cluster` and `Local` are handled, but Local requires ready EndpointSlices to identify their Nodes.
- Filters, headers, query parameters, methods, regular expressions, backend weights of zero, and cross-namespace references are rejected in status.
- GatewayClass requires the supported class `parametersRef`.
  Gateway `spec.infrastructure.parametersRef` overrides are rejected.
- Bound Gateway configuration is fixed for its UID, including after parameter changes or deletion.
  Use a new Gateway to apply different class settings.
- Existing OpenStack resources are never adopted.

## Safe removal

Keep the controller, its application credential, and OpenStack access running until every managed HTTPRoute and Gateway has finished finalization.
Removing the Deployment or Secret first can leave OpenStack resources behind and leave Kubernetes objects stuck in deletion.

For the basic example, delete in this order:

```sh
kubectl -n gateway-api-openstack-demo delete httproute basic
kubectl -n gateway-api-openstack-demo delete gateway edge
kubectl delete gatewayclass openstack
kubectl delete openstackgatewayclassconfig openstack
kubectl delete -f examples/basic.yaml --ignore-not-found
```

Each of the first three commands waits for deletion by default.
If one remains blocked, keep the controller Deployment, credential Secret, and OpenStack access available.
Inspect the object's conditions and controller logs, then use the ownership audit to compare immutable identity with OpenStack.
Do not remove controller finalizers manually because doing so bypasses ownership checks and cleanup.
Follow the [operator recovery guide](operator-recovery.md) for the complete procedure.

Before uninstalling a shared controller, repeat the HTTPRoute, Gateway, and GatewayClass deletion sequence for every class using its exact controller name.
Remove a parameter object only after no class needs it.
Keep the original controller and credentials available throughout this process.

The base uninstall also deletes the OpenStackGatewayClassConfig CRD, which removes every instance of that API throughout the cluster.
Check for remaining configuration objects:

```sh
kubectl get openstackgatewayclassconfigs.gateway-api-openstack.github.io
```

Only after the list is empty and no other installation needs the CRD, uninstall the base manifests:

```sh
kubectl delete -k config/default
```

If another installation uses this API, remove only the resources owned by this installation and retain the shared CRD and any shared RBAC resources.
The base uninstall removes the dedicated `openstack-gateway-system` namespace and its credential Secret.
It does not remove the upstream Gateway API CRDs.
