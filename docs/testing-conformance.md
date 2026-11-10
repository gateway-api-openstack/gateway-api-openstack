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

The [Core inventory](../test/design/core-mapping.json) groups the pinned GATEWAY-HTTP Core tests by requirement.
CI checks it against upstream source with `make test-core-mapping`.
The target downloads only the pinned source module and runs a local inventory check, without contacting Kubernetes or OpenStack.
An inventory match does not execute conformance tests or declare supported features.
Use [ADR 0006](design/adr/0006-core-model-boundaries.md) for accepted model boundaries and [ADR 0005](design/adr/0005-core-resource-mapping.md) for unresolved mappings and verification cases.

During development, follow the [incremental E2E plan](testing-openstack-e2e.md#validate-changes-incrementally) from a working traffic baseline to each implemented feature.
Select applicable upstream tests once their setup dependencies are met, then test interactions such as route precedence with redirects, invalid shares with weighted backends, and ReferenceGrant revocation with TLS rotation.
These checks can run before every Phase 4 task is complete.
Extend the test harness as behavior becomes available rather than assuming the current runner implements these scenarios.

Use the conformance module and Standard CRDs for the same pinned Gateway API release.
Before a full run:

1. Provide a controller configuration that accepts the upstream Service inputs, listeners, and namespace relationships.
2. Verify runner-to-Gateway and Amphora-to-backend connectivity, HTTPS credentials, and the approved cloud resource budget.
3. Isolate test resources and verify ownership and cleanup between runs using the [OpenStack E2E guidance](testing-openstack-e2e.md).
4. Run every Core test and every test for declared Extended features.
   Do not remove required behavior from fixtures to obtain a pass.
5. Retain the suite-generated report and reproduction details under the [publication policy](../CONTRIBUTING.md#test-evidence-and-publication).

Selecting one upstream test can still invoke shared setup that requires unsupported features.
Check the fixture and setup dependencies before choosing an incremental test.
An adapted diagnostic can help investigate behavior, but it does not establish profile conformance.
One-off runners and environment-specific adaptations remain local.
Record the gaps below to distinguish setup blockers, failures, and tests that have not run.

## Record conformance gaps

Keep exploratory records local under the [publication policy](../CONTRIBUTING.md#test-evidence-and-publication).
Identify whether the run used an upstream profile or an adapted diagnostic, then record the controller revision, Gateway API version, environment, command, selected profile or tests, feature flags, and original result artifact.
List fixture changes and setup blockers.
For tests against unreleased Octavia extensions, retain the exact controller, Octavia, octavia-lib, and Amphora build inputs privately with the run.
Identify the upstream changes and the dependency on unreleased code in any public summary.
Do not report that result as compatibility with an unmodified released cloud.
Rerun affected tests when those dependencies change, and run the complete required profile on the named released dependencies before closing Phase 5.

Summarize passed, failed, skipped, and setup-blocked tests within the selected scope.
Unselected tests have not run and do not count as skipped or passed.
For each failure, skipped test, or setup blocker, record the cause and a follow-up issue:

| Classification | Use for |
| --- | --- |
| Controller defect | Implemented behavior that does not meet its contract. |
| Routing translation work | Missing or incomplete compiler behavior. |
| Backend mode or topology requirement | Required Service inputs or connectivity not covered by the current design. |
| Octavia API limitation | A missing primitive established by cited API or implementation evidence. |
| Planned Gateway API extension | An Extended feature outside the current claim. |
| Intentional non-goal | Behavior outside the project's target profile. |
| Environment or harness failure | Setup, permissions, network, or test execution failures. |

Record the observed `GatewayClass.status.supportedFeatures` and explain differences from the test selection.
Do not change feature declarations merely to enable a test or hide a failure.
Link required implementation and design changes to the roadmap, and state whether the result is eligible for upstream submission.
This record does not replace the suite-generated conformance report.

## Publish results

Upstream lists [Conformant and Partially Conformant implementations](https://gateway-api.sigs.k8s.io/docs/implementations/list/).
This project's target requires the complete Core profile and all declared Extended features to pass.
An eligible partial report can support an earlier listing during Phase 5 development, but does not satisfy that target or close Phase 4.
The current adapted single-test diagnostic is not an eligible profile report, and this repository still needs a reusable report-producing runner.
The current empty `supportedFeatures` list also blocks the suite's default inferred-feature setup.
Resolve submission eligibility without declaring unverified features or overriding feature selection merely to qualify.

Before submitting a partial result, make the upstream setup work, publish a reproducible immutable evaluation version, and run the profile with known gaps explicitly identified.
Keep the suite-generated report unmodified and retain the reasons for skipped tests and earlier failures.
Do not weaken fixtures, relabel failed tests, or advertise unverified features to obtain a listing.
The [reporting rules](https://github.com/kubernetes-sigs/gateway-api/blob/a52aff438a5c133b10376fbe45c4096a2b0ce8ff/conformance/reports/README.md) require successful or partial profile results, not failed results.

Under the [conformance levels](https://gateway-api.sigs.k8s.io/docs/conformance/levels/) checked on 2026-11-06, full conformance uses one of the latest two minor releases and partial conformance uses one of the latest three.
Recheck these windows and the actual list generator when submitting, rather than relying on an older implementation's entry.
At the reviewed [upstream revision](https://github.com/kubernetes-sigs/gateway-api/tree/a52aff438a5c133b10376fbe45c4096a2b0ce8ff), profile validation adds a limit of five skipped tests to each partial Core or Extended status when evaluated as v1.7, while the list generator still evaluates against v1.6.
This transition is not a permanent allowance for broad skips.
Reassess version eligibility, skip limits, and feature declaration rules before scheduling the submission run.
The repository remains pinned to v1.6.1 until a separate dependency update is reviewed.

Submit the report and [ImplementationListDetails](https://github.com/kubernetes-sigs/gateway-api/blob/a52aff438a5c133b10376fbe45c4096a2b0ce8ff/conformance/list/implementations/README.md) upstream, and generate the list with `make update-implist` in the upstream repository.
Follow the [entry procedure](https://gateway-api.sigs.k8s.io/docs/implementations/list/#adding-new-entries) instead of editing the generated implementation page directly.
Phase 5 completion permits a full Conformant submission without waiting for Phase 6 operations or Phase 7 stability.
Listing review is independent of those project milestones and does not establish production readiness.
Keep `GatewayClass.status.supportedFeatures` limited to fully verified behavior.
Formal Gateway reports infer features from that status, as described in the [implementer's guide](https://gateway-api.sigs.k8s.io/guides/implementers-guide/#conformance-and-version-compatibility).
