# Gateway API conformance diagnostic

The opt-in runner in `test/conformance` uses Gateway API v1.6.1 to run the upstream `HTTPRouteSimpleSameNamespace` assertions against an existing controller.
It replaces the upstream base setup with one HTTP Gateway and a NodePort backend.
This is an adapted diagnostic, not a conformance profile result or an upstream submission.
No live result is recorded for the current revision.

## Scope

The separate Go module pins both Gateway API and its conformance module to v1.6.1.
It loads the upstream HTTPRoute fixture and invokes the upstream test function without changing its assertions.
The fixture names and namespace match that test.
The runner does not call the default suite setup, install a controller or CRDs, or change `GatewayClass.status.supportedFeatures`.

The default upstream setup uses ClusterIP backends and Gateways with listeners and namespace relationships outside the current controller's HTTP subset.
Selecting one upstream test still runs that shared setup in the normal suite.
This runner instead creates a dedicated namespace, one NodePort Service, one backend Deployment with one replica, one HTTP Gateway, and the selected HTTPRoute.
The Deployment selects a worker through the supplied labels.
The Service uses `externalTrafficPolicy: Local` to restrict members to Nodes with ready backend endpoints.

The installed controller can provision one Amphora load balancer and its listener, pool, member, health monitor, and L7 resources.
Octavia may also create the supporting Amphora resources.
The class must use `floatingIP.mode: None`.
The runner makes no direct OpenStack API calls and does not modify existing network resources.
This is a functional traffic check, not a load test.

## Prerequisites

Use a cluster and project approved for creating and cleaning up this test graph.
Prepare the following before opting in:

- Install the Gateway API v1.6.1 Standard CRDs, the class configuration CRD, and the controller as described in [Getting started](getting-started.md).
- Provide an accepted GatewayClass with a current `SupportedVersion=True` condition and an OpenStackGatewayClassConfig that uses no Floating IP.
- Verify routing from the test machine to the class's VIP subnet and from Amphora to the selected worker's NodePort.
- Select an eligible, ready worker through workload placement labels.
  The runner waits for the actual backend Pod and ready endpoint on a selected worker before creating the Gateway.
- Make the upstream `echo-basic` backend image available to that worker and pin its reference by digest.
  The image must implement the upstream echo response format.
- Leave `gateway-conformance-infra` absent.
  The runner refuses to adopt an existing namespace.
- Give the test identity permission to read cluster, class, CRD, Node, Pod, ReplicaSet, and EndpointSlice state and to create and clean up the namespaced fixture.
  The runner does not grant RBAC permissions.

The kubeconfig context, API server, `kube-system` namespace UID, GatewayClass UID, and class configuration UID identify the intended environment.
The runner verifies them before creating fixtures and reads the class again immediately before creating the Gateway.
Keep those values and the kubeconfig private.

## Prepare and run

Compile and test the harness without cluster or cloud access:

```sh
make test-conformance-compile test-conformance-unit
```

For a live diagnostic:

1. Copy [diagnostic.example.yaml](../test/conformance/diagnostic.example.yaml) to a private path.
2. Fill in the existing environment's identities, worker labels, and backend image reference.
   Replace `<sha256-digest>` with the image's actual digest.
3. Set `reportPath` to a new file in an existing private directory.
   The runner creates it with mode `0600` and refuses to overwrite a file.
4. Set `acceptResourceCreation: true` after approving the graph described above.
5. Run the diagnostic from the repository root:

   ```sh
   make test-conformance CONFORMANCE_CONFIG=/absolute/private/path/diagnostic.yaml
   ```

The Make target supplies the opt-in and configuration environment variables and a 75-minute Go test deadline.
The runner rejects a shorter remaining budget before creating resources so setup and upstream polling leave time for cleanup.
Ordinary unit, race, and CI checks leave live execution disabled.

## Cleanup and results

Cleanup verifies the run annotation and recorded UID, deletes the HTTPRoute, waits for controller finalization, then deletes the Gateway and waits again before removing the namespace.
It retains objects when creation is uncertain, identity changes, finalization stalls, or another Gateway, HTTPRoute, or LoadBalancer Service appears in the namespace.
Do not remove finalizers to unblock the test.
Follow [operator recovery](operator-recovery.md) if cleanup stops.

The private JSON report records the upstream version and revision, selected test, fixture adaptations, assertion result, and cleanup result.
`setupblocked` means the assertions did not run, while `pass` means only the selected assertions passed with those adaptations.
Successful Kubernetes deletion is not an independent OpenStack leak audit.
Use the [OpenStack E2E guide](testing-openstack-e2e.md) when that evidence is required.

Keep raw output and environment details local under the [test evidence policy](reports/README.md).
Use the [gap report template](reports/conformance-gap-template.md) to summarize findings and setup blockers.
Unselected tests have not run, and this diagnostic cannot justify a `supportedFeatures` declaration.
A full GATEWAY-HTTP assessment uses the upstream profile setup and all applicable Core tests.
A conformance claim requires those tests to pass.
