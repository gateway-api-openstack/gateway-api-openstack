# Gateway API conformance gap report: controller revision

Status: Draft

Date: YYYY-MM-DD

Gateway API version:

Controller release or commit:

Environment report:

## Invocation

Identify the run as an upstream profile or an adapted diagnostic.
Record the exact suite revision, selected tests or profile, feature flags, command, and original result artifact.
List fixture changes and setup blockers.
Keep exploratory results local under the [publication policy](README.md).
This template does not replace an upstream conformance report.

## Summary

| Result | Count |
| --- | ---: |
| Passed | 0 |
| Failed | 0 |
| Skipped | 0 |
| Setup blocked | 0 |

Count only selected tests.
Unselected tests have not run and do not count as skipped or passed.

## Gap classification

Classify each failed or skipped test and each setup blocker within the selected scope.

| Test | Result | Classification | Reason | Follow-up |
| --- | --- | --- | --- | --- |
| Example | Failed | Controller defect | Replace with the actual finding | Issue link |

Use one of these classifications:

- controller defect
- routing translation work
- backend mode or topology requirement
- Octavia API limitation
- planned Gateway API extension
- intentional non-goal
- environment or harness failure

## Feature claims

List the `GatewayClass.status.supportedFeatures` observed during the run.
Explain any difference between those runtime claims and the test selection.
Do not add a feature merely to make a test run, or remove one merely to hide a failure.

## Conclusion

State whether the result is eligible for upstream submission under the current Gateway API rules.
An adapted diagnostic is not a profile conformance result.
Link each required change to its roadmap item or design issue.
For an Amphora limitation, cite the API or implementation evidence and distinguish a current limitation from a feature that has not been investigated.
