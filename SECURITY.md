# Security policy

## Reporting a vulnerability

Please **don't** open a public issue. Report it privately through
[GitHub private vulnerability reporting](https://github.com/oprysok/acr-pull-secret-controller/security/advisories/new).

Include what you found, how to reproduce it, and the version (image tag or commit) affected. You should get a first response within a week.

## Supported versions

Only the latest release receives fixes.

## Verifying images

Release images are built by GitHub Actions and published with SLSA build provenance and an SBOM:

```bash
gh attestation verify oci://ghcr.io/oprysok/acr-pull-secret-controller:<version> --owner oprysok
```
