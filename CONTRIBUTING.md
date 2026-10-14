# Contributing

Thanks for helping! This guide gets you from clone to merged pull request.

## Set up

Option A — **Dev Container** (VS Code / Codespaces): open the repo and choose *Reopen in Container*. Everything is installed.

Option B — local: install Docker (with Compose v2), GNU Make 4 (on macOS: `brew install make`, then use `gmake`;
the stock 3.81 is refused because it ignores `.SHELLFLAGS`), [pre-commit](https://pre-commit.com/), and the tools of
the areas you work on:

| Area | Tools (CI versions) |
|---|---|
| `services/orders` | [uv](https://docs.astral.sh/uv/) 0.13 (Python 3.13 is installed by uv) |
| `services/fleet`, `services/simulator`, `tools/rollout-gate` | [Go](https://go.dev/dl/) 1.27, [golangci-lint](https://golangci-lint.run/) 2.14 |
| `services/console` | Node 24 |
| `infra/` | Terraform 1.16.5, tflint 0.64, trivy, checkov |
| `deploy/` | helm, kustomize, yq, kubeconform, kyverno, trivy, shellcheck (pinned in `.github/workflows/deploy.yml`) |
| `observability/` | sloth 0.16, promtool 3.x, python3 |

```bash
make setup   # dependencies of every area + git hooks
make check   # lint, type-check, static checks and tests of everything (what CI runs); add -k to see every failure
make up      # run the whole stack locally (cp .env.example .env first); make sim-up starts the city
```

Each area has its own targets (`make orders-test`, `make fleet-lint`, `make deploy-lint`, ...): run `make` to list
them, and see the README in each directory.

## Workflow

1. Create a short-lived branch from `main` (`feat/…`, `fix/…`).
2. Keep pull requests small and focused; one logical change each.
3. Write the PR title as a [Conventional Commit](https://www.conventionalcommits.org/) —
   `feat: add order cancellation`, `fix: retry webhook on 429`, `docs: …`. With squash merges the PR title becomes
   the commit message, and release-please uses it for the version bump and changelog
   (`feat` → minor, `fix` → patch, `feat!` / `BREAKING CHANGE` → major).
4. CI must be green and one code owner must approve. Each area has its own workflow (`orders.yml`, `fleet.yml`, ...)
   with no path filters, so every required check reports on every pull request.

## Standards

- Tests for every behaviour change; coverage must stay ≥ 80 %.
- Record significant decisions as an ADR in `docs/adr/` (copy `0000-template.md`).
- Update the runbook when you add an alert, and the architecture doc when you add a component.
- Change a contract (event schema, HTTP shape, Redis key) in [contracts/](contracts/) first, in the same pull
  request as the services that use it; never rename or remove a field without a new schema version.
- Never commit secrets — use `.env` locally (git-ignored) and a secret manager in the cloud.
