# Security policy

## Project status

The project is pre-alpha and does not yet have a supported production release.
This does not reduce the importance of reporting vulnerabilities responsibly.

| Version | Supported |
| --- | --- |
| No published release | No production support |
| Current `main` branch | Security fixes are considered on a best-effort basis |

## Reporting a vulnerability

Do not put vulnerability details in a public GitHub issue.

Check the [organization repository's security page](https://github.com/gateway-api-openstack/gateway-api-openstack/security) for the private vulnerability reporting button.
Use that private channel when it is available.
The project does not publish another monitored private security address.
If private reporting is unavailable, open an issue that says only that a private reporting channel is needed.
Do not include the vulnerability, affected resources, or reproduction details.
A maintainer must establish a private channel before asking for those details.

The repository owner must verify that private vulnerability reporting is enabled before publishing a release.
Record that check in the release issue.

Include the following information in a private report:

- affected version or commit
- impact and threat model
- reproduction details
- whether credentials, OpenStack project isolation, resource deletion, or traffic exposure is involved
- any suggested mitigation
- your preferred contact details and disclosure timeline

Never send real OpenStack credentials or customer data.
Use redacted configuration and a minimal reproduction.

## Response

Maintainers acknowledge private reports as soon as practical, assess affected versions, and coordinate a fix and disclosure.
The project will publish formal response times before its first production release.

## Scope

Security concerns include:

- Keystone credentials and Kubernetes Secrets
- cross-namespace references
- OpenStack project and resource isolation
- resource identity, adoption, and deletion
- OpenStack security group management
- TLS material and Barbican
- controller RBAC
- release images and the software supply chain
