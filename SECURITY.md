# Security Policy

## Reporting a vulnerability

If you believe you have found a security vulnerability in Cosmopilot, please report it
**privately**. Do not open a public GitHub issue for security problems.

Email **[dev@voluzi.com](mailto:dev@voluzi.com)** with:

- a description of the vulnerability and its impact;
- steps to reproduce, or a proof of concept;
- affected version(s) and environment details.

We will acknowledge your report, work with you to understand and validate the issue,
and keep you informed of progress toward a fix. Please give us a reasonable amount of
time to address the issue before any public disclosure.

## Supported versions

Security fixes are provided for the latest released version. We recommend always
running the most recent release.

## Security model & best practices

Cosmopilot is a Kubernetes operator that manages blockchain nodes and the secrets they
depend on. A few notes on how it is designed and how to run it safely:

- **Pod Security:** Cosmopilot enforces the Kubernetes **restricted** Pod Security
  profile by default — containers run as non-root, drop all Linux capabilities, and
  disable privilege escalation. See
  [Prerequisites](docs/docs/getting-started/prerequisites.md#container-image-requirements).
- **Admission webhooks:** validating webhooks are enabled by default and require
  cert-manager-issued certificates. Keep them enabled in production.
- **Key management:** consensus and account keys are stored in Kubernetes Secrets.
  For validators, prefer [TMKMS](docs/docs/usage/tmkms.md) with a remote signer
  (HashiCorp Vault) so private keys never live next to the node.
- **RBAC:** the operator runs with the minimum cluster permissions it needs, defined in
  the Helm chart's RBAC templates.
- **Exposing endpoints:** only expose node APIs (RPC, LCD, gRPC, EVM) that you intend
  to be reachable, and consider fronting them with
  [CosmoGuard](docs/docs/usage/cosmoguard.md) for access control and rate limiting.

Thank you for helping keep Cosmopilot and its users safe.
