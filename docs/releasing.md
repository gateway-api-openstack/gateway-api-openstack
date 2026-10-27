# Releasing gateway-api-openstack

This document describes the maintainer workflow for preparing release artifacts.
The project is pre-alpha.
A draft or published GitHub release does not by itself establish production readiness, OpenStack compatibility, or Gateway API conformance.
Release automation targets [gateway-api-openstack/gateway-api-openstack](https://github.com/gateway-api-openstack/gateway-api-openstack).

## Current release scope

GoReleaser v2.17.1 builds release bundles for these Linux architectures:

- `linux/amd64`
- `linux/arm64`

Each bundle contains `openstack-gateway-controller`, `openstack-gateway-audit`, `openstack-gateway-diagnostics`, `octavia-capability-probe`, `LICENSE`, `README.md`, and `SECURITY.md`.
It also includes the YAML files under `config/` and `examples/`, with their directory structure preserved.
These files include the OpenStackGatewayClassConfig CRD, controller installation manifests, and synthetic examples.
A release also contains a source archive and `checksums.txt` with SHA-256 checksums.

Public controller image publication and scheduled OpenStack E2E jobs are not configured.
They still need an agreed registry and a dedicated cloud test environment.
Helm charts, signatures, provenance, and SBOMs also remain roadmap work, with signing identity and promotion policy decisions still open.
Do not describe the binary bundle as a published controller image or claim those software supply chain guarantees.

## Repository settings

GitHub repository settings are separate from the workflow files.
Verify that Actions can run the pinned actions and that the main branch rules require `lint`, `race`, `verify`, and `envtest` from the CI workflow.
Keep tag protection consistent with the immutable release policy.

Check private vulnerability reporting, Dependabot security updates, secret scanning, and push protection under the [security policy](../SECURITY.md).
Repeat this review after a repository transfer.

## Validate locally

Use the Go version declared in `go.mod`, GoReleaser v2.17.1, and `kubectl` for local Kustomize rendering.
Start from a clean checkout with the complete Git history and tags, then run:

```sh
make verify
make build
make release-check
make release-snapshot
```

The snapshot command writes artifacts to `dist/` and never uploads them.
Its version is the next patch version followed by `-devel.` and an abbreviated commit SHA, so local artifacts cannot be confused with a tagged release.

Inspect the result before proposing a release:

```sh
ls dist
tar -tzf dist/gateway-api-openstack_v*_linux_amd64.tar.gz
(cd dist && shasum -a 256 -c checksums.txt)
```

Extract both architecture archives and render the manifests from the extracted files:

```sh
set -eu
release_dir="$(mktemp -d)"
for archive in dist/gateway-api-openstack_v*_linux_*.tar.gz; do
  tar -xzf "$archive" -C "$release_dir"
  bundle_dir="$release_dir/$(basename "$archive" .tar.gz)"
  for binary in openstack-gateway-controller openstack-gateway-audit openstack-gateway-diagnostics octavia-capability-probe; do
    test -x "$bundle_dir/$binary"
  done
  kubectl kustomize "$bundle_dir/config/crd" >/dev/null
  kubectl kustomize "$bundle_dir/config/default" >/dev/null
done
```

Each architecture archive must contain the files listed in [Current release scope](#current-release-scope).
The source archive and every binary archive must be covered by `checksums.txt`.
Rendering checks the packaged Kustomize paths without contacting a cluster.
Installation still requires operator credentials, approved infrastructure, and an image built from the release revision.

Support claims require validation of the proposed release revision under the [test evidence policy](reports/README.md).
Local packaging checks do not establish OpenStack compatibility or Gateway API conformance.

## Prepare a release

1. Open a **New release** issue and use it as the review record.
   Link to CI results, compatibility reports, tests run in OpenStack, and conformance results instead of making general support claims.
2. Select a SemVer version with a `v` prefix.
   Use a reviewed commit reachable from `main` as the release target.
3. Confirm the exact target commit is green in CI and repeat the local validation from a clean checkout.
4. Review README, the getting started guide, ROADMAP, `SUPPORT.md`, `SECURITY.md`, governance, and Amphora compatibility documentation.
   State missing evidence explicitly.

## Create the draft release

Create an annotated or signed tag after the release review is complete.
The workflow rejects lightweight tags, non-SemVer tags, and tags whose commit is not reachable from `origin/main`.
The following version is an example, not a published release:

```sh
git switch main
git pull --ff-only
version=v0.3.0-alpha.1
git tag -a "${version}" -m "Release ${version}"
git push origin "${version}"
```

Pushing the tag runs `.github/workflows/release.yml`.
The workflow checks module files and runs `make verify` before GoReleaser uploads the archives and checksums to a **draft** GitHub release.
It never publishes the release automatically.
Do not create the draft in advance.
Reuse an existing draft only when retrying one that this automation created.

The workflow does not read the Changelog section from the release issue.
Copy the reviewed text into the draft, preserve the pre-alpha warning, and edit the release notes to match the recorded evidence.

If the workflow fails before the draft is complete, fix the underlying cause on `main` and choose a new version.
Never move or reuse a tag that has been pushed.
If a retry encounters partial assets, delete only the unpublished draft release, keep the tag, and rerun the failed workflow.

## Review and publish

Before publishing the draft:

1. Compare the tag's commit with the approved release issue target.
2. Download the assets and verify `checksums.txt` independently.
3. Extract the archive for each supported architecture, verify its binaries, documents, configuration, and examples, and render its Kustomize manifests.
4. Review the generated changelog, pre-alpha warning, compatibility evidence and test results for the release, upgrade actions, and known limitations.
5. Confirm no artifact or release note contains credentials, private cloud identifiers, or claims beyond the recorded evidence.
6. Publish the draft manually and link it from the release issue.

Published tags and artifacts are immutable project history.
Correct a broken release with a new patch version.
Do not replace its tag or assets.
