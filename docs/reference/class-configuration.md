# GatewayClass configuration

OpenStackGatewayClassConfig supplies the infrastructure settings for new Gateways.
It is a cluster-scoped `v1alpha1` API in the group `gateway-api-openstack.github.io`.
GatewayClass must reference it through `spec.parametersRef`, with no namespace.
The API group and default controller identity use the organization's domain under [ADR 0003](../design/adr/0003-project-organization.md).
[ADR 0002](../design/adr/0002-class-configuration.md) defines class behavior and compatibility limits.

## Configure a class

The following example uses synthetic subnet IDs.
Replace them with existing subnets in the controller's authenticated OpenStack project.

```yaml
apiVersion: gateway-api-openstack.github.io/v1alpha1
kind: OpenStackGatewayClassConfig
metadata:
  name: openstack
spec:
  vipSubnet:
    id: 11111111-1111-4111-8111-111111111111
  memberSubnet:
    id: 22222222-2222-4222-8222-222222222222
  floatingIP:
    mode: None
  nodeAddressType: InternalIP
  healthMonitor:
    path: /
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openstack
spec:
  controllerName: gateway-api-openstack.github.io/gateway-api-openstack
  parametersRef:
    group: gateway-api-openstack.github.io
    kind: OpenStackGatewayClassConfig
    name: openstack
```

The controller name must exactly match the installation's `--controller-name` setting.
Credentials, project scope, cluster identity, region, and request limits remain [deployment settings](../getting-started.md#deployment-settings).
A parameter object cannot select different credentials or a different project.
Amphora and NodePort are the only provider and member mode.
There are no deployment topology flags or fallback settings for a GatewayClass without `parametersRef`.
Gateway `spec.infrastructure.parametersRef` is not supported.

## Fields and defaults

| Field | Default | Behavior |
| --- | --- | --- |
| `vipSubnet` | Required | Selects the existing subnet for the load balancer VIP. |
| `memberSubnet` | Required | Selects the existing subnet supplied to Octavia members. |
| `floatingIP.mode` | `None` | `None` publishes the VIP. `Allocate` creates a Floating IP owned by the Gateway. |
| `floatingIP.externalNetwork` | Unset | Required for `Allocate` and forbidden for `None`. Selects an existing external network. |
| `nodeAddressType` | `InternalIP` | Selects `InternalIP` or `ExternalIP` from eligible Nodes. |
| `flavorID` | Unset | Selects an enabled Amphora flavor by UUID. |
| `availabilityZone` | Unset | Selects an enabled Amphora availability zone by exact name. |
| `healthMonitor.path` | `/` | HTTP path starting with `/`, with no control characters and at most 255 characters. |
| `healthMonitor.delay` | `10` | Interval in seconds, from 1 through 3600. |
| `healthMonitor.timeout` | `5` | Timeout in seconds, from 1 through 3600 and less than `delay`. |
| `healthMonitor.maxRetries` | `3` | Successful checks required for recovery, from 1 through 10. |
| `healthMonitor.maxRetriesDown` | `3` | Failed checks required to mark a member down, from 1 through 10. |

Health monitors use HTTP `GET` and accept status codes `200-399`.
Those settings are fixed.
There is no option to disable the health monitor or supply an existing Floating IP.

Each subnet or external network reference must contain exactly one selector:

| Selector | Matching rule |
| --- | --- |
| `id` | One UUID. |
| `name` | An exact name of 1 through 255 characters, with no surrounding whitespace or control characters. |
| `tags` | All listed tags must match. Supply 1 through 16 unique tags, each 1 through 60 characters, with no commas, surrounding whitespace, or control characters. |

For example, a subnet reference can use `name: gateway-vip` or `tags: [gateway-vip, development]` instead of `id`.
Names and tags must resolve to exactly one resource across all returned pages.
Zero matches, multiple matches, or a response that contradicts the selector rejects the configuration.

Both subnets must belong to the authenticated project and use the same IP version.
Their parent networks must be administratively enabled and list the selected subnets.
An external network must be visible to the credential, administratively enabled, and marked external.
It may belong to the cloud operator's project, as shared public networks often do.
Floating IP allocation currently requires an IPv4 VIP subnet.
Reading these resources does not give the controller ownership of them.

## Validation and permissions

GatewayClass validation uses read-only Octavia and Neutron calls.
It checks the selected topology, advertised Octavia API version, Amphora availability, and any requested flavor or availability zone.
Unknown or unverifiable capabilities are not accepted.

| Configuration | Minimum configured Octavia microversion | Additional reads |
| --- | --- | --- |
| Base HTTP and NodePort profile | `2.5` | API version discovery, provider list, selected subnets, and their networks. |
| `flavorID` | `2.6` | Flavor and flavor profile, including proof that the profile uses Amphora. |
| `availabilityZone` | `2.14` | Availability zone and its profile, including proof that the profile uses Amphora. |
| `floatingIP.mode: Allocate` | `2.5` | Selected external network. |

The configured microversion must also be advertised by the cloud.
The base Deployment uses `--octavia-microversion=2.5`.
Raise that value when using optional fields and verify the cloud supports it.
Some clouds restrict flavor or availability zone profile reads.
If the controller cannot read the required profile, it cannot validate that option.

`Accepted=True` means these reads and configuration checks succeeded.
It does not prove permission to create or delete resources, available quota, backend reachability, or Gateway API conformance.
The resolver does not perform trial writes or change Keystone policy.
The deployment credential still needs the Octavia and Neutron permissions required to reconcile its owned graph.
Provisioning failures use fixed conditions and Events without publishing OpenStack response bodies.

Missing parameters, invalid selectors, or unsupported capabilities produce `Accepted=False` with `InvalidParameters` on GatewayClass.
Authentication and authorization failures preserve the last evaluated `ObservedGeneration` and retry slowly after reporting a redacted diagnostic.
Correct the configuration or restore the required access, then let reconciliation retry.

## Configuration after Gateway creation

Before creating cloud resources, the controller records the resolved IDs, health monitor settings, Node address type, and parameter object identity in the Gateway's durable binding.
It persists that binding with its finalizers and returns before provisioning.
The resulting configuration remains fixed for that Gateway UID.

Editing the parameter object or changing its selectors affects Gateways that have not yet acquired a binding.
It does not move an existing Gateway to another subnet, flavor, availability zone, or Floating IP policy.
Deleting the parameter object makes the referencing GatewayClass invalid for new bindings, but an already bound Gateway continues to use its stored configuration.
Its cleanup does not require the parameter object to exist.

The binding also survives replacement of an owned graph for the same Gateway UID, including a listener port change or cleanup after an invalid Gateway spec.
Correcting the spec rebuilds the graph with the original configuration.
To use different class settings, let the old Gateway finish deletion and create a new Gateway with a new UID.
Do not edit the controller's binding annotations or finalizers to change topology.

A partial or changed binding stops mutation with an ownership conflict.
Recovery requires the original identity and intact binding, as described in the [operator recovery guide](../operator-recovery.md).
Deleting the entire configuration CRD is different from deleting one parameter object: the running controller still requires the API for its watches.
Keep the CRD installed until all installations using it have been removed.

## Worker NodePort members

A Node is eligible only when it is ready, schedulable, not being deleted, and has a valid IP address of the configured `nodeAddressType`.
The presence of either `node-role.kubernetes.io/control-plane` or `node-role.kubernetes.io/master` excludes it, regardless of the label's value.
A `node-role.kubernetes.io/worker` label is not required and cannot override a control plane label.
There is no control plane opt-in.

The `node.kubernetes.io/exclude-from-external-load-balancers` label applies to the remaining Nodes:

| Label value | Membership |
| --- | --- |
| Absent | Eligible if the other checks pass. |
| A false boolean value | Eligible if the other checks pass. |
| Empty, true, or invalid | Excluded. |

Boolean parsing accepts Go's standard spellings, including `false`, `False`, `FALSE`, `f`, `F`, and `0` as false.
Use `"false"` when an explicit value is needed.

Both supported traffic policies require at least one ready, nonterminating backend endpoint.
With `externalTrafficPolicy: Cluster`, all eligible workers can become members.
With `externalTrafficPolicy: Local`, a worker must also have a ready endpoint identified by `EndpointSlice.endpoints[].nodeName`.
The route remains pending when no worker is eligible.
Node and EndpointSlice changes recalculate membership, preserving members whose address and NodePort have not changed.

Operators must ensure that the selected Node addresses belong to the configured member network and that Amphora can reach their NodePorts.
This also applies when selecting `ExternalIP` instead of the default `InternalIP`.
The topology checks do not verify that path, and the controller does not create backend security groups or attach groups to worker ports.

## Development compatibility

The API group, default controller name, and binding format are pre-alpha contracts.
There is no automatic binding migration or resource adoption path.
Before changing the API group, controller name, cluster identity, or project scope, finalize the development resources with the original controller and credentials, then recreate the installation and its resources.
