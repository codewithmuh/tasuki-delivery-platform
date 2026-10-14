# Security policy

## Reporting a vulnerability

Please **do not open a public issue**. Report privately through
[GitHub private vulnerability reporting](https://github.com/codewithmuh/tasuki-delivery-platform/security/advisories/new)
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
| Dependency and base-image updates every week | `.github/dependabot.yml`; Helm charts in `deploy/` via `renovate.json` |
| Vulnerability and misconfiguration scans (repo, every image, rendered manifests, Terraform) | `ci.yml` and each area's workflow (Trivy, checkov) |
| GitHub Actions pinned to commit SHAs, least-privilege tokens | `.github/workflows/*` |
| No AWS keys in CI: OIDC, a read-only plan role, an apply role gated by a reviewer | `infra/bootstrap/github-oidc.tf` |
| Containers run as non-root, read-only, all capabilities dropped | `Dockerfile`s, `compose.yaml`, `deploy/charts/service`, Kyverno policies |
| Signed multi-arch images (cosign, keyless) with SBOM and provenance | `release.yml` |
| Personal data encrypted per field, never in events or logs | `services/orders` |

The STRIDE review and the accepted risks are in [docs/threat-model.md](docs/threat-model.md).

Verify a published image (every service: `orders`, `fleet-ingest`, `fleet-state`, `tracking`, `dispatch`,
`routing`, `simulator`, `console`, `rollout-gate`):

```bash
cosign verify ghcr.io/codewithmuh/tasuki-delivery-platform/orders:<version> \
  --certificate-identity https://github.com/codewithmuh/tasuki-delivery-platform/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
