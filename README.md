# Tasuki — the open-source delivery platform you run in your own cloud

[![ci](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/ci.yml/badge.svg)](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/ci.yml)
[![release](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/release.yml/badge.svg)](https://github.com/codewithmuh/tasuki-delivery-platform/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

<img width="2172" height="724" alt="tasuki-github-cover-q75" src="https://github.com/user-attachments/assets/eb94039c-3550-4cec-88b6-6926bb4cea20" />

Tasuki (襷, the sash that ekiden relay runners hand to each other) is **the delivery side of a business, as software
you deploy next to your own**. Your system creates a delivery over the API; Tasuki quotes a price and an ETA, finds
the nearest driver, streams the driver's position live to your customer's phone, and reports every status change
back. It is open source (MIT), runs in your cloud account, and is built to production standards: idempotent orders,
a Kafka-backed location pipeline, written SLOs, load tests and game days. It is sized for **1.5 million orders a
month in one city**, and [measured](docs/loadtests/README.md) carrying that design's rainy-Friday peak (18,144
orders/h, 641 GPS points/s, 2,707 live tracking pages) on one laptop with every software SLO met.

> **Status: early, and honest about it.** The order, tracking and fleet path runs today; the [roadmap](#status) says
> what comes next. The reference deployment is **Tokyo's 23 wards on real Tokyo roads**, and the merchants, drivers
> and customers in it are simulated. Making zones, currency and tax configurable for any city is the next phase
> ([ADR 0020](docs/adr/0020-service-area-as-configuration.md)). Landing page: [website/](website/).

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

**Phase 1 of 7 is done: the platform is finished and proven locally.** Every service is written, tested in CI and
runs with one command; the whole order-to-delivery path has [published load-test numbers](docs/loadtests/README.md)
up to the full design scale, and the first game day ([GD-01](docs/gameday/experiments/01-redis-loss.md#results)) ran
with a [postmortem](docs/postmortems/2026-10-16-gd-01-local-redis-loss.md) whose fixes shipped the same day. The AWS
platform (Terraform) and the GitOps layer (Argo CD) are written and statically checked; running them on a real
cluster is deliberately the **last** phase, once the product is ready for it.

| Phase | Scope | Status |
|---|---|---|
| **1. Finish and prove** | Dispatch; the whole order-to-delivery path under load, with published numbers; the first game day | Done (locally) |
| **2. Any city** | Zones from a GeoJSON file, server-side zone lookup, configurable currency, tax and map region ([ADR 0020](docs/adr/0020-service-area-as-configuration.md), proposed) | Next |
| **3. Connectable** | Signed webhooks, a dashboard with login (orders, drivers, manual assignment, API keys), an API docs site | Planned |
| **4. Driver app** | A web app (PWA) first; native later for background GPS | Planned |
| **5. Easy install** | A single-server setup and a published Helm chart | Planned |
| **6. AI ops copilot, then depth** | An assistant that explains late orders with read-only tools; then batched dispatch, delivery windows, the Temporal saga, ledger and payouts, partner OAuth2 | Planned |
| **7. Production on AWS** | The first run of the cluster in `infra/` and `deploy/`, and the game days that need it | Last |

Phase 1, part by part:

| Part | Where | State |
|---|---|---|
| orders: quotes, idempotent orders, outbox relay, dispatch consumer | [services/orders](services/orders/) | Built, tested (incl. PostgreSQL races) |
| fleet-ingest, fleet-state, tracking | [services/fleet](services/fleet/) | Built, tested |
| dispatch (greedy, P1) | [services/fleet](services/fleet/) | Built, tested; held the design-scale load test |
| routing: OSRM on Tokyo roads | [services/routing](services/routing/) | Built; image built on `main` only (slow) |
| simulator: the city, including customers who watch their tracking page | [services/simulator](services/simulator/) | Built, tested |
| console: live map + tracking page | [services/console](services/console/) | Built, tested |
| SLOs, alerts, rollout gate, dashboards | [observability/](observability/), [tools/rollout-gate](tools/rollout-gate/) | Built, promtool-tested |
| AWS platform (VPC, EKS, Karpenter, OIDC) | [infra/](infra/) | Written, statically checked; not applied yet |
| GitOps (Argo CD, charts, policies, data stores) | [deploy/](deploy/) | Written, rendered and policy-checked; no cluster run yet |
| Load tests | [docs/loadtests/](docs/loadtests/), [scripts/loadtest](scripts/loadtest/) | Two published runs on one laptop, up to the full design scale |
| Game days, postmortems | [docs/gameday/](docs/gameday/), [docs/postmortems/](docs/postmortems/) | GD-01 run locally, with a postmortem; the other drills need the cluster (phase 7) |

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
| [website/](website/) | The public landing page: static HTML, CSS and JS, no build step |
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
| [docs/loadtests/](docs/loadtests/README.md) | Published load-test results, and how to reproduce them |
| [docs/gameday/](docs/gameday/) | Game-day plans and results |
| [docs/postmortems/](docs/postmortems/README.md) | Postmortems of incidents and game days |
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
