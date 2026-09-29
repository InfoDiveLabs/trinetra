# Security Policy

## Supported versions

| Version | Supported          |
| ------- | ------------------ |
| 0.5.x   | Yes                |
| ≤0.4.x  | No (serverwatch-era; upgrade) |

Only the current `0.5.x` line receives security fixes. Releases up to and
including v0.4.1 (published as serverwatch) are unsupported; see
[Upgrading from a serverwatch install](docs/handbook/03-installation.md#upgrading-from-a-serverwatch-install)
to move to a supported release.

## Reporting a vulnerability

Please do **not** open a public GitHub issue for a security problem.

Report it through GitHub's private vulnerability reporting: open **"Report a
vulnerability"** under this repository's **Security** tab, or go directly to
<https://github.com/InfoDiveLabs/trinetra/security/advisories/new>. This
creates a private draft advisory visible only to you and the maintainers,
with no public disclosure until a fix is ready.

When reporting, please include, as far as you're able to:

- A description of the issue and its impact.
- The affected version (`trinetra version`, `trinetra-ctl version`, or the
  release tag).
- Steps to reproduce, or a proof of concept.
- Whether it affects the core daemon, a plugin (`trinetra-web`,
  `trinetra-ctl`), or the release-signing/self-update pipeline.

## What to expect

We will acknowledge a new report within a few business days. A confirmed
vulnerability is fixed and shipped as a normal signed release; the advisory
is published once hosts have had a reasonable chance to update
(coordinated disclosure). We do not currently commit to a fixed remediation
timeline beyond that acknowledgement.

## Scope

This policy covers:

- The core `trinetra` daemon and its collectors.
- The official plugins, `trinetra-web` and `trinetra-ctl`.
- The release-signing and self-update pipeline (`cmd/trinetra-release`, the
  manifest format, and `trinetra update` / `trinetra install
  --require-signed`).

For the full trust model — what trinetra trusts, how a release is signed,
and how to verify a release's signature by hand — see the handbook's
[Security chapter](docs/handbook/14-security.md#verify-a-download-yourself).
