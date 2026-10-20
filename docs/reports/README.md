# Test evidence and publication

Commit tests and documentation that remain useful as the implementation develops.
Summarize validation in the PR and retain detailed output in local or CI artifacts.
Ordinary development runs do not need a committed report.
Release compatibility and conformance claims require reviewed public evidence.

## What goes where

| Material | Default location | Public content |
| --- | --- | --- |
| Regression tests, synthetic fixtures, reusable runners, and execution instructions | Repository | Checks that protect a continuing contract and can run in another contributor's environment. |
| Temporary diagnostic code, timing instrumentation, environment setup scripts, and notes | Ignored local directory | Include only a reusable regression or fix derived from the investigation. |
| Validation for an ordinary change | PR testing section | Command, tested scope, results, cleanup, material limitations, and a CI link when available. |
| Generated reports, logs, metrics, and resource snapshots | Ignored local directory or reviewed CI artifacts | The redacted excerpt needed to explain a result or failure. |
| Internal cloud and cluster names, private endpoints and IDs, populated configuration, and local image references | Private local run record | Omit unless an authorized detail is necessary to understand a defect. |
| Credentials, tokens, kubeconfig, certificates, and credential-bearing dumps | Protected files outside the report bundle | Never publish in Git, PR text, or CI artifacts. |
| Release compatibility decisions and conformance submissions | Reviewed public report | Relevant versions, topology, public release artifact identity, results, limitations, and durable supporting links. |

Judge test code by the behavior it protects, not the phase in which it was written.
A test for a lasting invariant belongs in the repository.
A diagnostic that must be discarded when a temporary implementation restriction disappears stays local.
Do not keep an unsupported behavior as a public regression expectation merely because a development run observed it.

Pin test images by digest and retain those digests locally for reproducibility.
Include a digest in the public summary only when reviewers can access the image and its identity matters to the claim or defect.

## Minimum PR evidence

For OpenStack runs, use the [validation template](openstack-e2e-template.md).
For local tests, a concise command and result summary is sufficient.
Identify local changes so the result is not attributed to an untested PR revision.
Include unresolved failures even if a later retry passed.
Link a public CI run when available, or state that the checks ran locally and details are retained privately.
Do not link private filesystem paths in a public PR.

## Local and CI artifacts

Use `.local/` for private tools and notes that must survive `make clean`.
Use `_artifacts/` or `test-results/` for disposable generated output.
These directories are ignored by Git, but ignore rules do not restrict file access.
Keep credentials and sensitive raw output in protected storage outside a report bundle.
Retain the evidence needed through review and any related investigation.

For shared projects, compare independent inventories before and after the test and account for changes before claiming cleanup is complete.
The PR needs the outcome and any limitation, not tenant inventories or other users' resource counts.

CI artifacts may be public and may expire.
Review contents and access settings before uploading.
Preserve public release evidence for as long as the project makes the claim it supports.
[Prow documents automatic artifact upload and the risk of leaking secrets through public logs and artifacts](https://docs.prow.k8s.io/docs/components/pod-utilities/).

## Related guidance

The [Cluster API contribution guide](https://github.com/kubernetes-sigs/cluster-api/blob/main/CONTRIBUTING.md#triaging-pr-or-periodic-test-failures), [CAPO CI guide](https://cluster-api-openstack.sigs.k8s.io/development/ci#prow), and [cloud-provider-openstack CI runner](https://github.com/kubernetes/cloud-provider-openstack/blob/master/tests/ci-occm-e2e.sh) illustrate PR summaries backed by separate test artifacts.
The rules for internal cloud names and local image references are this project's publication policy.

Use the [conformance plan](../testing-conformance.md) to select a profile and the [gap template](conformance-gap-template.md) to classify findings.
Adapted diagnostics remain development evidence and do not replace the suite report required for upstream submission.
See the [release guide](../releasing.md) for release evidence.
