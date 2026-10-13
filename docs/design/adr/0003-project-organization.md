# ADR 0003: Project organization and API identity

Status: Accepted

Date: 2026-10-09

Owner: `jihyun-huh`

Approval: The repository owner transferred the project to the `gateway-api-openstack` GitHub organization and explicitly selected `gateway-api-openstack.github.io` as its API domain.
This scoped decision supersedes the initial development identity.
It does not change the class configuration, topology binding, or worker selection contracts.

## Context

Phase 3 initially used a domain under the maintainer's personal GitHub account.
The project now has its own organization and repository at [gateway-api-openstack/gateway-api-openstack](https://github.com/gateway-api-openstack/gateway-api-openstack).
Its Go module, Kubernetes API, controller default, and repository links need to identify that project consistently.

## Decision

Use `github.com/gateway-api-openstack/gateway-api-openstack` as the Go module path and canonical source repository.
Use `gateway-api-openstack.github.io` as the API group for OpenStackGatewayClassConfig and `gateway-api-openstack.github.io/gateway-api-openstack` as the default controller name.
The API version remains `v1alpha1`.
Update the matching CRD name, RBAC, manifests, examples, imports, tests, and installation instructions together.

Controller binding keys remain derived from the configured controller identity.
Changing the default does not authorize adopting resources created under another identity.
An explicit controller name remains available for isolated installations and tests.

Release automation identifies the new repository as its publication destination.
Security reporting uses that repository's private vulnerability reporting channel when enabled.
The organization move does not create a published image, enable a hosted website, grant another person a maintainer role, or establish a support commitment.
Keep the named human maintainers in `OWNERS` until an explicit role change is approved.

## Compatibility and validation

This pre-alpha change adds no compatibility alias, automatic API conversion, or cloud identity rewrite.
Finish cleanup with the original controller and its identity before replacing an existing development installation.
Then install the new CRD and recreate configuration and Gateway resources as needed.
Never remove finalizers to bypass cleanup.

Validate module resolution, controller and schema tests, envtest, installation rendering, and release packaging with the new names.
Local validation does not prove that GitHub organization settings, private vulnerability reporting, or a published release are configured.
Live OpenStack and conformance evidence remain separate release gates.
