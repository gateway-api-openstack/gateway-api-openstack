# Contributing to gateway-api-openstack

The project welcomes bug reports, test results, design discussions, and focused code changes.

Please read the [code of conduct](code-of-conduct.md) before participating.
Project roles and the decision process are described in [GOVERNANCE.md](GOVERNANCE.md).

## Before you start

Search the issue tracker before opening a new issue.
Use the bug report for a controller defect, the environment report for results from an Amphora cloud, and the feature request for a new behavior.

Small fixes can go straight to a pull request.
Changes to public APIs, ownership, resource mapping, or other contracts listed in the [ADR process](docs/design/adr/README.md) need a design issue and an accepted decision before implementation.

If you are looking for a first contribution, issues labeled `good first issue` should be possible without access to an OpenStack cloud.
Issues labeled `help wanted` are expected to have enough context for somebody outside the current maintainer group to pick them up.
Ask on the issue if that is not true.

## Set up a development environment

Clone the canonical project repository:

```sh
git clone https://github.com/gateway-api-openstack/gateway-api-openstack.git
cd gateway-api-openstack
```

Use `github.com/gateway-api-openstack/gateway-api-openstack` for imports within this module.

Use the Go version in `go.mod`.
Unit tests use fake Kubernetes clients or fake OpenStack HTTP servers and do not need cloud credentials.

Run the normal checks with:

```sh
make verify
make test-race
```

Controller tests that need API server behavior use envtest:

```sh
make envtest-assets
make test-envtest
```

The first command downloads the pinned Kubernetes control plane binaries.
The envtest suite installs the Gateway API Standard Channel CRDs pinned by this repository.
It does not run Octavia, send traffic, or provide Gateway API conformance evidence.

Run `make lint` when `golangci-lint` is installed.
Use `make fmt` for Go source formatting.
Do not run `make manifests`, `make generate`, or Kubebuilder scaffolding commands.
This repository consumes upstream Gateway API types and maintains its class configuration API, CRD, Kustomize, and RBAC files by hand.

Never put `clouds.yaml`, application credential secrets, tokens, certificates, private addresses, project IDs, or customer information in an issue, test fixture, log, or pull request.

## Make a change

Keep a pull request focused.
A refactor should normally be separate from a behavior change, especially in reconciliation, finalization, and ownership code.
Add the Apache 2.0 boilerplate from `hack/boilerplate` to new Go files.

Follow the dependency boundaries in the [development layout guide](docs/development-layout.md).
Keep Kubernetes reconciliation in controller packages and Gophercloud calls in the OpenStack adapter.

When a reconciler starts reading or writing another Kubernetes resource, update both its RBAC markers and `config/rbac/cluster_role.yaml`.
Preserve status fields owned by other controllers.

Every cloud lifecycle change should cover the relevant create, converged no-op, restart, partial graph, external deletion, drift, ownership conflict, and repeated deletion cases.
Count mutating provider calls where a minimal diff is part of the contract.

## Open a pull request

Fill in the pull request template and include:

- the user or operator problem
- the important design choices
- exact tests run
- relevant, redacted environment details when cloud behavior was tested
- API, ownership, compatibility, and conformance impact

All required CI checks must pass.
Follow the [publication policy](docs/reports/README.md) to separate reusable tests and PR summaries from local configuration, temporary diagnostics, and detailed run output.

Reviewers check correctness and test coverage.
Approvers check the wider API, ownership, compatibility, and project direction.
The root `OWNERS` file is the current source for those roles.
Until the project has more than one approver, independent maintainer approval is not always possible.
Record that limitation openly rather than adding inactive names.

## Documentation

Write documentation in U.S. English and use the Kubernetes documentation style: direct sentences, sentence case headings, and standard Kubernetes and Gateway API terminology.
Describe current behavior separately from planned behavior.
Put each complete sentence on its own source line without wrapping it to a fixed width.
Preserve code blocks, tables, and syntax-significant indentation.
Do not use “supported,” “conformant,” or “production-ready” unless the matching evidence is published.

## License

By contributing, you agree that your contribution is licensed under the [Apache License 2.0](LICENSE).
The project does not currently run the CNCF CLA bot.
A future move into a community organization may require contributors to complete that organization's contribution process.
