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

Each class selects network placement and Node addresses through an OpenStackGatewayClassConfig.
See the [configuration reference](reference/class-configuration.md) for the fields and [worker eligibility rules](reference/class-configuration.md#worker-nodeport-members).

The controller does not create networks, subnets, routers, or backend security groups and does not attach security groups to worker ports.
Establish that connectivity before creating a Gateway.

Install the pinned Standard Channel CRDs if the cluster does not already have them:

```sh
kubectl apply -f \
  https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml
```

Startup requires GatewayClass, Gateway, and HTTPRoute to be available.
The controller validates installed bundle annotations through the GatewayClass `SupportedVersion` condition.
A version mismatch blocks provisioning and repair without preventing finalization, as described in the [API version contract](design/architecture.md#installed-gateway-api-version).

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
Before changing an existing installation's identity, follow the [compatibility guidance](reference/class-configuration.md#development-compatibility).

Configure VIP and member placement in the OpenStackGatewayClassConfig in `examples/basic.yaml`.
Replace its synthetic subnet IDs with existing resources in the controller project.
The [class configuration reference](reference/class-configuration.md#fields-and-defaults) covers Floating IP selection, health settings, and optional placement.

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

At the default resync interval, observations occur about 8 to 12 minutes apart.
Stable jitter based on the resource UID spreads the OpenStack requests.
Set the environment variable or use `--openstack-resync-interval` to change the base interval.
The command line flag takes precedence, and the value must be greater than zero.

Every OpenStack service client in the process shares one request budget.
This also covers authentication and token renewal, so increasing controller worker counts cannot bypass the limit by using another service client.
Both request settings must be greater than zero.
The equivalent command line flags are `--openstack-api-qps` and `--openstack-api-burst`.

The base Deployment also sets these binary flags: leader election enabled, metrics on `:8080`, probes on `:8081`, and Octavia microversion `2.5`.
Optional flavor, availability zone placement, and referenced frontend security groups require higher microversions and additional profile reads, as described in the [configuration reference](reference/class-configuration.md#validation-and-permissions).
The binary additionally supports `--openstack-operation-timeout` and `--openstack-poll-interval`.
The manifests do not override these two flags, so the binary uses defaults of 10 minutes and 2 seconds.
The timeout limits each provider reconciliation call, including all OpenStack requests made by that call.
After starting an asynchronous Octavia change, the controller returns and observes it again after the poll interval instead of waiting for it to finish in the same reconciliation.

## Inspect network prerequisites

Use the read-only diagnostic tool after installing an OpenStackGatewayClassConfig and before creating test Gateways:

```sh
make build-diagnostics
bin/openstack-gateway-diagnostics \
  --class-config=openstack \
  --kubeconfig=<path-to-kubeconfig> \
  --context=<context-name> \
  --clouds-yaml=<path-to-clouds.yaml> \
  --openstack-cloud=<cloud-name> \
  --openstack-region=<region> \
  --octavia-microversion=<controller-microversion> \
  > environment-diagnostics.json
```

Use the same credential scope, region, and microversion as the controller.
The Kubernetes identity needs get access to the selected OpenStackGatewayClassConfig and list access to Nodes.
`--node-selector` can narrow the worker selection.
`--check-barbican` adds a metadata access check without reading secret payloads.
The default deadline is two minutes, with the same 10 QPS and burst 20 defaults as the controller.

The command inspects the current class template, not an existing Gateway's frozen binding.
It checks label-eligible workers, including unready Nodes, without claiming the member eligibility of a particular Service.
Results distinguish `passed`, `failed`, `unknown`, and `skipped` checks.
Update permissions, actual backend reachability, Amphora topology, and the kube-proxy implementation remain unknown because these reads cannot establish them.
Observed IP families describe selected Node addresses, not verified IPv6 or dual-stack support.

The JSON report omits resource names, cloud IDs, addresses, credentials, and raw API errors.
Exit status 0 means no observed failure, 2 means at least one failed check, and 1 means invalid command options or an output error.
Unknown checks still require evidence even when the command exits successfully.
This report does not replace traffic tests or the [ownership audit](operator-recovery.md#run-the-ownership-audit).

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

See the [supported routing scope](../README.md#current-implementation) and [class configuration lifecycle](reference/class-configuration.md#configuration-after-gateway-creation) before extending this example.

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
If one remains blocked, keep the installation available and follow the [operator recovery guide](operator-recovery.md).
Do not remove controller finalizers manually because doing so bypasses ownership checks and cleanup.

Before uninstalling a shared controller, repeat the HTTPRoute, Gateway, and GatewayClass deletion sequence for every class using its exact controller name.
Remove a parameter object only after no class needs it.

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
