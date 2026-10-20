# Gateway API conformance

The target is Gateway API v1.6.1 Standard Channel and the `GATEWAY-HTTP` profile.
Its [Core feature set](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/conformance/utils/suite/profiles.go) contains Gateway, HTTPRoute, and ReferenceGrant, including HTTPS termination for HTTPRoute.
The repository does not yet provide a full-profile runner.

## Implementation gaps

This table describes implementation coverage, not test results.
The [compatibility record](providers/compatibility.md) summarizes the available evidence.

| Area | Current implementation | Remaining Core work |
| --- | --- | --- |
| Listeners | One HTTP listener without a listener hostname | Multiple listeners and their lifecycle, hostname matching, HTTPS, and certificate validation |
| Route attachment | One selected HTTPRoute with one parent in the Gateway namespace | Multiple routes and parents, namespace selection, and precedence |
| Matching | One rule and match, Exact or PathPrefix, and at most one exact hostname | Multiple rules and matches, wildcard and multiple hostnames, and exact header matching |
| Service inputs | One NodePort Service in the route namespace | ClusterIP and headless Services, and manually managed EndpointSlices |
| Backend selection and errors | One backend | Weighted backends, weight zero, unmatched-request HTTP 404, and required responses for omitted or invalid backends |
| Filters | No filters | RequestHeaderModifier and Core RequestRedirect behavior |
| References | References within the route namespace | ReferenceGrant for Service and certificate references |
| Partial validity and status | Conditions and listener status for the supported subset | Required behavior for invalid siblings, reference errors, listener changes, and generation updates |

Requiring NodePort Services is a Core gap, even though NodePort can be used internally as a forwarding mechanism.
Rejecting unsupported input does not satisfy a requirement for positive traffic behavior or a particular HTTP response.
The [feasibility assessment](design/conformance-feasibility.md) identifies Octavia API blockers and designs that need verification.
The [roadmap](../ROADMAP.md) assigns that work and defines the completion gates.

Method and query matching, URLRewrite, response header modification, mirroring, retries, timeouts, and `GatewayHTTPListenerIsolation` are outside the initial Extended claim.
Other Route profiles and mesh conformance are also outside the target.

## Prepare the suite

Use the conformance module and Standard CRDs for the same pinned Gateway API release.
Before a full run:

1. Provide a controller configuration that accepts the upstream Service inputs, listeners, and namespace relationships.
2. Verify runner-to-Gateway and Amphora-to-backend connectivity, HTTPS credentials, and the approved cloud resource budget.
3. Isolate test resources and verify ownership and cleanup between runs using the [OpenStack E2E guidance](testing-openstack-e2e.md).
4. Run every Core test and every test for declared Extended features.
   Do not remove required behavior from fixtures to obtain a pass.
5. Retain the suite-generated report and reproduction details under the [publication policy](reports/README.md).

Selecting one upstream test can still invoke shared setup that requires unsupported features.
Check the fixture and setup dependencies before choosing an incremental test.
An adapted diagnostic can help investigate behavior, but it does not establish profile conformance.
One-off runners and environment-specific adaptations remain local.
Use the [gap template](reports/conformance-gap-template.md) to distinguish setup blockers, failures, and tests that have not run.

## Publish results

Upstream lists [Conformant and Partially Conformant implementations](https://gateway-api.sigs.k8s.io/docs/implementations/list/).
This project's target requires the complete Core profile and all declared Extended features to pass.
A partial report can document progress but does not satisfy that target.

Follow the pinned [reporting rules](https://github.com/kubernetes-sigs/gateway-api/blob/v1.6.1/conformance/reports/README.md) for an unmodified report, an immutable implementation version, reproduction instructions, and implementation metadata.
Recheck version eligibility when submitting upstream.
Keep `GatewayClass.status.supportedFeatures` limited to fully verified behavior.
Formal Gateway reports infer features from that status, as described in the [implementer's guide](https://gateway-api.sigs.k8s.io/guides/implementers-guide/#conformance-and-version-compatibility).
