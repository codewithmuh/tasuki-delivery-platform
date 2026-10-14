# Security policy

## Reporting a vulnerability

Please **do not open a public issue**. Report privately through
[GitHub private vulnerability reporting](https://github.com/codewithmuh/production-repo-template/security/advisories/new)
or email **contact@codewithmuh.com** with:

- what is affected (service, version or commit),
- how to reproduce it,
- the impact you expect.

You will get an acknowledgement within **3 business days** and a plan within **10 business days**.
Fixes are released as a patch version and credited in the release notes unless you prefer otherwise.

## Supported versions

Only the latest release receives security fixes.

## How this repository protects itself

| Control | Where |
|---|---|
| Secret scanning on every commit and pull request | `.pre-commit-config.yaml`, `ci.yml` (gitleaks) |
| Dependency and base-image updates every week | `.github/dependabot.yml` |
| Vulnerability and misconfiguration scans (repo + images) | `ci.yml` (Trivy) |
| GitHub Actions pinned to commit SHAs, least-privilege tokens | `.github/workflows/*` |
| Containers run as non-root, read-only, all capabilities dropped | `Dockerfile`s, `compose.yaml` |
| Signed images (cosign, keyless) with SBOM and provenance | `release.yml` |

Verify a published image:

```bash
cosign verify ghcr.io/codewithmuh/production-repo-template/api-go:<version> \
  --certificate-identity-regexp 'https://github.com/codewithmuh/production-repo-template/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
