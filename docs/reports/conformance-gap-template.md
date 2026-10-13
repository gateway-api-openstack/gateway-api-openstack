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
This is a local gap analysis unless an accepted upstream report says otherwise.

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
- compiler work
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
List the work needed for the next report and the Core semantics that may be structurally unrepresentable on Amphora.
