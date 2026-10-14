# Tasuki — a Tokyo delivery platform, built production-grade

[![ci](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/ci.yml/badge.svg)](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/ci.yml)
[![release](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/release.yml/badge.svg)](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

> **Tasuki is a fictional company.** The merchants, couriers, customers and orders are simulated. The services,
> the platform on AWS Tokyo, the SLOs, the load tests and the incidents are real.

Tasuki (襷, the sash that ekiden relay runners hand to each other) is a **delivery-as-a-service platform for Tokyo's
23 wards**. Merchants send delivery requests by API. Tasuki quotes a price and an ETA, assigns a courier, shows the
courier live on a map, settles the money, and pays couriers every week.

The repository designs the whole platform and builds a **thin slice through every hard problem** in it:

| Problem | How it is solved here |
|---|---|
| Live fleet data at ~750 GPS updates/s | Go ingest → Kafka → Redis cache rebuilt from the log; history in S3 as Parquet |
| Real-time dispatch | Matching per ward on real Tokyo roads (OSRM), lease-based ownership with fencing |
| Orders that are never lost or double-charged | Idempotency keys, transactional outbox, Temporal saga with compensation |
| Money that always balances | Double-entry ledger in integer yen, daily reconciliation that must sum to zero |
| Booking delivery windows (時間帯指定) | Contended capacity with no overbooking under concurrent load |
| Surges and partial failures | Honest quotes when demand outruns couriers, priority load shedding, graceful degradation |
| Operating it | EKS in ap-northeast-1, GitOps, OpenTelemetry, SLOs with burn-rate alerts, game days, postmortems |

**Start with the [system design](docs/design.md)**: scale assumptions, the architecture, failure modes, SLOs and
what is deliberately out of scope.

## Status

🚧 **Design phase.** The sample services from the
[production repo template](https://github.com/codewithmuh/production-repo-template) are still here and will be
replaced by Tasuki's services as each phase lands.

| Phase | Scope | Status |
|---|---|---|
| **P1: platform + first slice** | Terraform EKS in Tokyo, Argo CD, observability, SLOs, Go rollout gate; orders, fleet ingest, live tracking, OSRM, city simulator, live map | In progress |
| **P2: AI ops copilot** | An assistant that explains late orders using read-only tools, served through a separate LLM gateway repo | Planned |
| **P3: domain depth** | Batched dispatch, Temporal saga, ledger + reconciliation + payouts, delivery windows, partner API + webhooks | Planned |


## Documentation

| Document | What it covers |
|---|---|
| [docs/design.md](docs/design.md) | The full system design |
| [docs/architecture.md](docs/architecture.md) | What is built today and how the parts connect |
| [docs/adr/](docs/adr/) | Architecture decisions and why |
| [docs/runbooks/](docs/runbooks/) | One page per alert |
| [docs/threat-model.md](docs/threat-model.md) | STRIDE review |

## Quick start (current sample services)

Requirements: [uv](https://docs.astral.sh/uv/), [Go](https://go.dev/dl/),
[golangci-lint](https://golangci-lint.run/), [pre-commit](https://pre-commit.com/), Docker, or the Dev Container.

```bash
make setup    # install dependencies and git hooks
make check    # lint, type-check and test everything (same as CI)
make up       # run the services locally
make down
```

Run `make` with no arguments to list every command.

## Production-grade checklist

The project is announced as v1.0 only when every box is ticked.

- [ ] Code quality: linters, formatters, type checks and pre-commit pass
- [ ] Tests: unit + integration, ≥ 80 % coverage on core logic, a smoke test and a load test with published numbers
- [ ] CI: every PR runs lint, tests, build and scans; `main` is protected; Terraform plan posted on PRs
- [ ] Security: no secrets in git, OIDC to AWS, least privilege, scanned and signed images, SBOM, threat model
- [ ] Infrastructure as code: remote state, `make up` / `make down`, cost estimate, teardown leaves nothing billable
- [ ] Releases: semantic versions, changelog, images in GHCR
- [ ] Observability: logs, metrics, traces, dashboards as code, alerts linked to runbooks
- [ ] Reliability: probes, graceful shutdown, timeouts/retries/idempotency, tested restore, written SLOs
- [ ] Docs: this README, design, architecture, ADRs, runbooks, contributing, license
- [ ] Developer experience: one-command setup, Dev Container or Compose, seed data
- [ ] Demo: screenshots, demo video, write-up

## License

[MIT](LICENSE) © 2026 Muhammad Rashid ([CodeWithMuh](https://youtube.com/@codewithmuh))
