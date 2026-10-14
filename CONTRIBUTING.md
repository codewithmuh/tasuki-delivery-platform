# Contributing

Thanks for helping! This guide gets you from clone to merged pull request.

## Set up

Option A — **Dev Container** (VS Code / Codespaces): open the repo and choose *Reopen in Container*. Everything is installed.

Option B — local: install [uv](https://docs.astral.sh/uv/), [Go 1.27](https://go.dev/dl/),
[golangci-lint](https://golangci-lint.run/), [pre-commit](https://pre-commit.com/) and Docker, then:

```bash
make setup   # dependencies + git hooks
make check   # lint, type-check and test everything (what CI runs)
make up      # run all services locally
```

## Workflow

1. Create a short-lived branch from `main` (`feat/…`, `fix/…`).
2. Keep pull requests small and focused; one logical change each.
3. Write the PR title as a [Conventional Commit](https://www.conventionalcommits.org/) —
   `feat: add order cancellation`, `fix: retry webhook on 429`, `docs: …`. With squash merges the PR title becomes
   the commit message, and release-please uses it for the version bump and changelog
   (`feat` → minor, `fix` → patch, `feat!` / `BREAKING CHANGE` → major).
4. CI must be green and one code owner must approve.

## Standards

- Tests for every behaviour change; coverage must stay ≥ 80 %.
- Record significant decisions as an ADR in `docs/adr/` (copy `0000-template.md`).
- Update the runbook when you add an alert, and the architecture doc when you add a component.
- Never commit secrets — use `.env` locally (git-ignored) and a secret manager in the cloud.
