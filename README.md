# Tasuki — a Tokyo delivery platform, built production-grade

[![ci](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/ci.yml/badge.svg)](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/ci.yml)
[![release](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/release.yml/badge.svg)](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

<img width="2172" height="724" alt="tasuki-github-cover-q75" src="https://github.com/user-attachments/assets/eb94039c-3550-4cec-88b6-6926bb4cea20" />

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

**P1 in progress.** Every P1 service is written and tested in CI and runs locally with one command. The AWS
platform (Terraform) and the GitOps layer (Argo CD) are written and statically checked, but have not been applied to
a cluster yet. The template's sample services are gone.

| Phase | Scope | Status |
|---|---|---|
| **P1: platform + first slice** | Terraform EKS in Tokyo, Argo CD, observability, SLOs, Go rollout gate; orders, fleet ingest, live tracking, OSRM, city simulator, live map | In progress |
| **P2: AI ops copilot** | An assistant that explains late orders using read-only tools, served through a separate LLM gateway repo | Planned |
| **P3: domain depth** | Batched dispatch, Temporal saga, ledger + reconciliation + payouts, delivery windows, partner API + webhooks | Planned |

P1, part by part:

| Part | Where | State |
|---|---|---|
| orders: quotes, idempotent orders, outbox relay, dispatch consumer | [services/orders](services/orders/) | Built, tested (incl. PostgreSQL races) |
| fleet-ingest, fleet-state, tracking | [services/fleet](services/fleet/) | Built, tested |
| dispatch (greedy, P1) | [services/fleet](services/fleet/) | In progress |
| routing: OSRM on Tokyo roads | [services/routing](services/routing/) | Built; image built on `main` only (slow) |
| simulator: the city | [services/simulator](services/simulator/) | Built, tested |
| console: live map + tracking page | [services/console](services/console/) | Built, tested |
| SLOs, alerts, rollout gate, dashboards | [observability/](observability/), [tools/rollout-gate](tools/rollout-gate/) | Built, promtool-tested |
| AWS platform (VPC, EKS, Karpenter, OIDC) | [infra/](infra/) | Written, statically checked; not applied yet |
| GitOps (Argo CD, charts, policies, data stores) | [deploy/](deploy/) | Written, rendered and policy-checked; no cluster run yet |
| Game days, load tests, postmortems | [docs/gameday/](docs/gameday/) | Plans only; no drill run yet |

## Quick start (local stack)

Requirements: Docker with Compose v2, and **GNU Make 4** (macOS ships 3.81, which the Makefile refuses because it
ignores `.SHELLFLAGS`: `brew install make`, then type `gmake` wherever this page says `make`). Or open the repository
in the Dev Container, which has everything.

```bash
cp .env.example .env   # local placeholder secrets; read the comments to generate real ones
make up                # build and start PostgreSQL, Kafka, Valkey and every service; waits until healthy
                       # then open http://localhost:8088: the live map (empty until the city runs)
make sim-up            # start the simulator: it seeds merchants and couriers, then orders flow
make routing-up        # optional: OSRM on real Tokyo roads (first build ~10-20 min, 4-6 GB RAM)
make logs              # follow everything;  make ps  shows health
make down              # stop (data kept);   make reset  also deletes the local data
```

| Port (127.0.0.1) | What |
|---|---|
| [8088](http://localhost:8088) | console: live map (`/`, `/?ward=shibuya`) and tracking page (`/track/<token>`) |
| 8000 | orders API ([openapi.yaml](services/orders/openapi.yaml)) |
| 8081 · 8082 · 8083 · 8084 | fleet-ingest · dispatch · tracking · fleet-state |
| 8090 · 5000 | simulator · routing (OSRM), when started |
| 5432 · 6379 · 9094 | PostgreSQL · Valkey · Kafka, for tools on the host (`KAFKA_BROKERS=localhost:9094`) |

Without OSRM, dispatch and the simulator fall back to straight-line distances, so everything works, just less
precisely. To follow one delivery as a customer, open an order's `tracking_url`
(`http://localhost:8088/track/<token>`). Scenarios: `SIM_SCENARIO=rainy-friday make sim-up` (see
[services/simulator](services/simulator/)).

Developing: `make setup` installs every area's dependencies and the git hooks; `make check` runs the same lint,
type checks, static checks and tests as CI (it needs each area's tools; the Dev Container has the common ones).
Run `make` with no arguments to list every command.

## Repository layout

| Path | What it is |
|---|---|
| [services/orders](services/orders/) | Orders and money path: Django + DRF, PostgreSQL, transactional outbox to Kafka |
| [services/fleet](services/fleet/) | Location path, one Go module: fleet-ingest, fleet-state, tracking, dispatch |
| [services/routing](services/routing/) | OSRM with a bicycle graph of Tokyo's 23 wards |
| [services/simulator](services/simulator/) | The city: merchants, customers and couriers through the public APIs |
| [services/console](services/console/) | Live map and tracking page: React, TypeScript, MapLibre |
| [contracts/](contracts/) | Event schemas, HTTP shapes, Redis keys, ward reference data |
| [infra/](infra/) | Terraform: GitHub OIDC roles, VPC, EKS, Karpenter's AWS side |
| [deploy/](deploy/) | Everything Argo CD applies: platform add-ons, policies, data stores, service chart |
| [observability/](observability/) | SLO specs (Sloth), alert rules, Grafana dashboards as code |
| [tools/rollout-gate](tools/rollout-gate/) | Go service that blocks canaries when an error budget is spent |
| [docs/](docs/) | Design, architecture, threat model, runbooks, SLO policy, game days, ADRs |
| `mk/` | Make targets per area, included by the root [Makefile](Makefile) |

## Documentation

| Document | What it covers |
|---|---|
| [docs/design.md](docs/design.md) | The full system design |
| [docs/architecture.md](docs/architecture.md) | What is built today and how the parts connect |
| [contracts/README.md](contracts/README.md) | The agreements between services: events, APIs, Redis keys |
| [infra/README.md](infra/README.md) | The AWS platform: setup, daily `aws-up` / `aws-down`, cost, safety guards |
| [deploy/README.md](deploy/README.md) | GitOps: app of apps, secrets, services, policies, versions |
| [observability/slo/README.md](observability/slo/README.md) | The SLOs, their SLIs and burn-rate alerts |
| [docs/slo/error-budget-policy.md](docs/slo/error-budget-policy.md) | What happens when an error budget runs low or out |
| [docs/runbooks/](docs/runbooks/) | One page per alert |
| [docs/gameday/](docs/gameday/) | Game-day plans |
| [docs/threat-model.md](docs/threat-model.md) | STRIDE review and accepted risks |
| [docs/adr/](docs/adr/) | Architecture decisions and why |

### Before the first deploy to AWS

`make aws-up` and the GitOps bootstrap need a few things that cannot live in git (details in the linked READMEs):

- GitHub repository variables and the `aws-tokyo` environment for the CI roles ([infra/README.md](infra/README.md)).
- The secrets `tasuki/dev/app` and `tasuki/prod/app` in AWS Secrets Manager **before** `make gitops-bootstrap`;
  without them the services are not created ([deploy/README.md](deploy/README.md#secrets)).
- The GHCR packages made **public** after the first release (GHCR creates them private), or an `imagePullSecret`
  in each namespace.
- `make aws-bootstrap` creates the PostgreSQL backup bucket (it survives `make aws-down`); re-run it in an account
  bootstrapped before backups existed ([deploy/README.md](deploy/README.md#backups-and-restore)).
- Optional: the secret `tasuki/monitoring/alertmanager` (key `slack_webhook_url`) to send alerts to Slack; without
  it Alertmanager starts with a null receiver.

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
