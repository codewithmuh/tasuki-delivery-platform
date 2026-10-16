# Game days

- Status: **First drill run locally: [GD-01](experiments/01-redis-loss.md#results) on 2026-10-16 (Compose). No cluster drill yet.**
- Owner: Muhammad Rashid ([@codewithmuh](https://github.com/codewithmuh))
- Last updated: 2026-10-16

> Every "Results" section in [experiments/](experiments/) is an empty placeholder until the drill has actually been
> run. Timelines, numbers and postmortems are written afterwards, from the drill log, never before.

## What a game day is here

A game day is a **planned, time-boxed experiment**. It injects one failure from
[design §10](../design.md#10-failure-modes-and-degradation) into the running platform, with the
[simulator](../../services/simulator/README.md) providing the traffic. Each experiment starts from a written hypothesis
that says what users see, which SLIs move and by how much, which alerts fire, and which runbook applies. The drill
checks the hypothesis, and it also checks the runbook: a runbook that has never been followed under pressure has not
been tested.

It is not a load test, and it is not chaos on a timer. One person (or a small team) runs one failure at a time, with the
rollback written down before the fault goes in.

Each game day produces:

1. A filled-in **Results** section in the experiment plan.
2. A **drill log** with a timestamped timeline (`scripts/gameday/record-timeline.sh`), the raw material for everything else.
3. **Runbook, alert and dashboard fixes** for whatever was wrong or missing.
4. A **postmortem** when the [error-budget policy](../slo/error-budget-policy.md) requires one, or when the drill
   surprised us ([docs/postmortems/](../postmortems/README.md)).

## Coverage: every row of design §10

| §10 failure | Experiment | Where | Runnable today? |
|---|---|---|---|
| Redis lost | [GD-01 Redis loss](experiments/01-redis-loss.md) | dev, then prod | Yes |
| Rain surge (×3 demand) | [GD-02 Rainy-Friday surge](experiments/02-rainy-friday-surge.md) | prod | Yes |
| (error-budget policy, design §9) | [GD-03 Bad deploy blocked by the error-budget gate](experiments/03-error-budget-gate.md) | prod | Yes: Phase A (burn the budget, the gate says no) and Phase B (a canary aborted by the gate) |
| One AZ lost | [GD-04 Node loss and AZ impairment](experiments/04-node-and-az-loss.md) | dev (node), prod (AZ) | Yes |
| Kafka unavailable | [GD-05 Kafka broker loss](experiments/05-kafka-broker-loss.md) | dev (full outage), prod (one broker) | Yes |
| PostgreSQL primary fails | [GD-06 PostgreSQL primary failover](experiments/06-postgres-failover.md) | prod (2 instances) | Yes in prod; dev has one instance, so it becomes a restart drill |
| (secrets, design §11) | [GD-07 Broken secret](experiments/07-broken-secret.md) | dev | Yes |
| Region lost (backups) | [GD-08 Orders database backup and restore](experiments/08-orders-db-backup-restore.md) | dev | Yes: Phase A (logical copy) and Phase B (restore from the S3 backups, point in time) |
| OSRM down | [GD-09 Routing (OSRM) down](experiments/09-osrm-down.md) | dev | Yes |
| Dispatch replica dies | [GD-10 Dispatch replica loss](experiments/10-dispatch-replica-loss.md) | dev | Yes. P1 replicas own Kafka partitions and keep state in Valkey; per-ward leases and fencing arrive in P3 |
| Payment provider slow or down | [GD-11 Payment provider outage](experiments/11-payment-provider-outage.md) | dev | **Blocked**: the saga and the mock provider are P3 |
| Region lost (failover) | [GD-12 Region loss](experiments/12-region-loss.md) | — | **Blocked**: designed, not built (design §2, §10) |

## Where drills run, and what that means for SLOs

There is one EKS cluster with two namespaces, `tasuki-dev` and `tasuki-prod` (ADR 0016 pending). Both get their
traffic from the simulator; there are no real users in either.

| | `tasuki-dev` | `tasuki-prod` |
|---|---|---|
| Replicas | 1 of everything; 1 Kafka broker; 1 PostgreSQL instance | 2 per service (fleet-ingest and tracking 2–6 with HPAs; orders, tracking, fleet-ingest and dispatch are Argo Rollouts behind the error-budget gate); 3 Kafka brokers (RF 3, min ISR 2); PostgreSQL primary + 1 standby |
| SLO rules, budgets, burn alerts | **None.** Every Sloth spec selects `namespace="tasuki-prod"` | Yes |
| Cause alerts | Most do not fire: their rules select `tasuki-prod`. A few cover dev too and are routed to the ticket channel (dev never pages): `ExternalSecretSyncFailing`, `OrdersDatabaseBackupFailing`, and kube-prometheus-stack's upstream alerts (`KubePodNotReady`, ...) | Yes. Pages (`severity=page` or upstream `critical`) go to the Slack page channel, tickets to the ticket channel |
| Use it for | First runs of every drill: does the injection work, does the rollback work, what do the SLIs do | Anything that tests alerts, budgets, the rollout gate, or high availability |

So a dev drill shows how the system behaves (`watch-slis.sh --namespace tasuki-dev` computes the same SLIs for dev),
but no SLO alert can fire there. A drill whose hypothesis is about alerts has to run in `tasuki-prod`, inside a
scheduled window, with `ALLOW_PROD=yes`.

**Nodes are shared.** Both namespaces' pods land on the same Karpenter nodes, so a node or AZ drill always touches both
environments. `node-loss.sh` lists the `tasuki-prod` pods on the target nodes and refuses unless the drill is declared
as a prod drill.

### The error budget during drills

The [policy](../slo/error-budget-policy.md) is clear: **game days count**. Schedule them only while more than 50 % of
the service's budget is left, and run none on a service that has burned 75 %. `preflight.sh --service <service>`
checks this before a prod drill.

One property of this project changes the arithmetic. **The cluster is ephemeral** (`make aws-up` / `make aws-down`,
[infra/README.md](../../infra/README.md)), and Prometheus keeps its data on a volume in the `monitoring` namespace,
which `aws-down` deletes. Sloth's 28-day ratio averages the 5-minute ratios that exist, so in practice the "28-day"
budget covers only the hours since the cluster came up. A minute of failures in a two-hour-old cluster can spend the
whole orders budget. Consequences:

- **Run one damaging prod drill per service per session**, after the cheaper dev runs. Services have separate budgets:
  a Kafka drill (location path) and a PostgreSQL drill (orders) can share a session.
- **Run GD-03 last.** It spends the orders budget on purpose, which freezes orders for the rest of the session.
- **Report two numbers in every result and postmortem.** First, the ratio the policy defines (remaining budget before
  and after, read from `slo:period_error_budget_remaining:ratio`). Second, a **28-day equivalent**:

  ```text
  28-day-equivalent budget burned = bad events during the drill
                                    ÷ ((1 − objective) × events expected in 28 days at the scenario's rate)
  ```

  Take the events expected in 28 days from the scenario's demand curve (for orders availability: quotes plus order
  creations). The first number is what the policy acts on. The second is what the drill would have cost a real Tasuki
  with a full window.
- **The policy still applies as written.** A drill that burns more than 20 % of the remaining budget needs a postmortem,
  even when the 28-day equivalent is small. Whether ephemeral clusters should be judged differently is an open
  question for the policy owner (see [open questions](#open-questions-and-decisions-needing-an-adr)), not something a
  drill decides.

## Safety rules

1. **Explicit target, every time.** Every script needs `--context` and `--namespace`, has no defaults, accepts only
   `tasuki-dev` or `tasuki-prod`, and refuses `tasuki-prod` unless `ALLOW_PROD=yes` is set in the environment.
2. **Prod only in a dedicated window.** Announce it first: open a GitHub issue titled
   `Game day: GD-0X <name> (<env>, <date> <time> JST)` with the plan link, the window, the abort criteria and the rollback.
   Close it with the results link.
3. **Preflight must say GO** (`scripts/gameday/preflight.sh`): Argo CD synced and healthy, no pods failing, data
   stores healthy, the simulator running, no page alerts firing, and (prod) more than 50 % of the target service's budget left.
4. **One fault at a time, one driver at the keyboard.** Never stack a second experiment on an unrecovered first one.
5. **The rollback is ready before the fault goes in.** Every injection script has `--rollback` or a documented undo.
   Run it once in `tasuki-dev` before using it in `tasuki-prod`.
6. **Do not fight Argo CD.** Every object Argo CD manages has `selfHeal: true`, so `kubectl edit/scale/set env` on a
   managed object is reverted within seconds and leaves no trace in git. Injections therefore delete pods (controllers
   recreate them), act on data (Valkey, PostgreSQL), or act outside the cluster (AWS Secrets Manager, EC2). Changes to
   managed configuration (the simulator's scenario, the tracking brownout) go through a pull request, listed in the
   plan's preconditions. When a drill must hold a manual change on **one service**, pause that service's Application
   (`<service>-<env>`) as [deploy/README.md, "Game days"](../../deploy/README.md#game-days) shows, record the pause and
   the resume in the drill log, and check at the end that nothing is still paused. Only the per-service Applications can
   be paused; Kafka, PostgreSQL and Valkey settings belong to `tasuki-<env>` and always go through git. Objects a drill
   creates carry the label `tasuki.io/gameday=true` and are deleted when it ends.
7. **Never print secrets.** `broken-secret.sh` handles values only in memory and in a mode-600 temporary file, and logs
   key names and version IDs only. Drill logs are committed, so they must never contain a token, password or key.
8. **Mind the bill.** Karpenter may add nodes during a surge (NodePool limit: 32 vCPU). End the session with `make aws-down`.

### Abort criteria (all experiments)

Stop the experiment, run its rollback, and record `abort` in the drill log when any of these happens. Each plan adds
its own.

- A namespace, service or data store that the plan did not list starts to degrade.
- **Any sign of lost or duplicated orders**: after recovery, `simulator_orders_created_total` and the orders service's
  `orders_created_total` diverge by more than the in-flight count, or outbox rows disappear without being published.
  That is an incident, not a drill: follow [service-unavailable](../runbooks/service-unavailable.md) and write a postmortem.
- The rollback does not restore steady state within 10 minutes. Treat it as a real incident from that moment.
- Observability is lost (Prometheus or Grafana down): you can no longer see what the fault does.
- The time box in the plan is exceeded by 50 %.
- The AWS budget alert fires, or the Karpenter NodePool reaches its CPU limit.

## Roles

| Role | Does | Solo version |
|---|---|---|
| **Driver** | Runs preflight, the injection and the rollback; the only person typing commands that change things | You, in terminal 1 |
| **Observer** | Watches the dashboards and `watch-slis.sh`, compares them with the expected observations, calls the abort | `watch-slis.sh` in terminal 2; set a timer for each "by now you should see" checkpoint in the plan |
| **Scribe** | Writes the timeline as it happens: what was done, what was seen, when each alert fired | `record-timeline.sh note "..."` in terminal 3; the injection scripts log their own actions to the same file |

Solo drills also benefit from a screen recording, which makes the timeline easy to check afterwards.

## Tooling

### Scripts ([scripts/gameday/](../../scripts/gameday/))

All of them are bash, `set -euo pipefail`, and shellcheck-clean (`make gameday-lint`); `make gameday-test` proves
their guards without a cluster. Every script prints what it will do and asks before doing it (`--yes` skips the
prompt for automation). Read-only scripts ask too, so they are never pointed at the wrong cluster by accident. The one
exception is `record-timeline.sh note`/`mark`, which only appends to a log whose creation was confirmed.

| Script | What it does | Undo |
|---|---|---|
| `preflight.sh` | Read-only go/no-go: Argo CD, pods, data stores, simulator, firing alerts, budget > 50 % (prod) | — |
| `watch-slis.sh` | Read-only: prints the key SLIs every N seconds from the Prometheus HTTP API (port-forward) | — |
| `record-timeline.sh` | `start` a drill log, add a `note` or a phase `mark`, take a `snapshot` of the SLIs | — |
| `redis-loss.sh` | Deletes `redis-0` or runs `FLUSHALL`; times the rebuild | `--rollback`: restart fleet-state to force a rebuild |
| `kafka-broker-loss.sh` | Deletes one, several or all Strimzi broker pods; waits for recovery | Strimzi recreates them; `--rollback` waits and reports |
| `pg-failover.sh` | CloudNativePG switchover, or deletes the primary pod; times the failover | `--rollback`: switch back to the original primary |
| `node-loss.sh` | Drains a node or a zone, deletes a NodeClaim, or terminates an instance | `--rollback`: uncordon |
| `pod-kill.sh` | Deletes N pods of one service (routing, tracking, dispatch, ...) | The Deployment recreates them; `--rollback` waits for it |
| `broken-secret.sh` | Removes a key from, or corrupts a key in, `tasuki/<env>/app` in Secrets Manager; forces a sync | `--rollback`: moves `AWSCURRENT` back to the saved version |
| `orders-5xx.sh` | Revokes `INSERT` on `orders_order` from the app role, so order creation returns 5xx for `--duration` | Restores itself on exit; `--rollback` re-grants |
| `gate-check.sh` | Read-only: runs the rollout gate's `check` against the live budget | — |
| `pg-restore.sh` | GD-08 Phase A: copies the orders database into the cluster `orders-db-restore` (logical import) and times it | `--rollback`: deletes the copy |
| `restore-check.sh` | Read-only: row counts and checksums per table, and a comparison of two clusters | — |

Environment variables: `ALLOW_PROD=yes` (required for `tasuki-prod`), `GAMEDAY_LOG` (the drill log; injection
scripts append their actions to it), `PROM_URL` (use an existing Prometheus URL instead of a port-forward).

### Local drills ([scripts/gameday/local/](../../scripts/gameday/local/))

The cluster is deliberately the last phase of the roadmap, so the first drills run on the local Compose stack
(`make up`) with the simulator providing traffic and viewers. The services, their code paths and their failure
handling are the same as in the cluster; what a local drill cannot test is anything Kubernetes or AWS does (pod
rescheduling, PodDisruptionBudgets, Karpenter, Multi-AZ), and there are no SLO alerts or budgets, because there is no
Prometheus. The local scripts sample the services' `/metrics` and readiness endpoints directly and write a drill log
under `docs/gameday/logs/`.

| Script | Local twin of | What it does |
|---|---|---|
| `local/redis-loss.sh` | GD-01, `redis-loss.sh` | `--mode restart` stops and restarts the Valkey container (no persistence, so it comes back empty); `--mode flushall` empties it in place. Prints a one-second timeline until fleet-state has rebuilt the cache and dispatch has resumed |

A local result is recorded in the experiment's Results section as **"local (Compose)"**. The cluster run in
`tasuki-dev` and `tasuki-prod` still happens in phase 7; it is the one that tests alerts and budgets.

### Dashboards

Generated into `observability/dashboards/generated/` and loaded by the `grafana-dashboards` Application. The plans
refer to them by title: **Tasuki overview**, **Tasuki location pipeline**, **Tasuki orders & outbox**,
**Tasuki SLOs**, **Tasuki simulator** and **Tasuki service RED**. The Strimzi and CloudNativePG charts install their
own dashboards too. `watch-slis.sh` prints the same core numbers in a terminal, which is what goes into the drill log.

```bash
kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80   # Grafana (password: deploy/README.md, Observability)
kubectl -n monitoring port-forward svc/prometheus-operated 9090                # Prometheus: queries, ALERTS
kubectl -n monitoring port-forward svc/alertmanager-operated 9093              # Alertmanager
```

Alertmanager sends alerts to Slack ([deploy/README.md, "Alerting"](../../deploy/README.md#alerting)):
`tasuki-dev` always to the ticket channel, `severity=page` or upstream `critical` to the page channel, the rest to
the ticket channel. Drills check both that an alert fires (the `ALERTS` series, which `watch-slis.sh` prints) and that
its message arrived. Without the webhook secret, alerts still fire but reach nobody: check
`kubectl -n monitoring get externalsecret alertmanager-slack` before a drill that is about paging.

## Running a drill

**Before (the day before, or at the start of the session)**

1. Read the plan end to end. Check its preconditions, including any pull request it needs (simulator scenario, brownout).
2. Run the injection and the rollback once in `tasuki-dev`.
3. For prod: open the announcement issue.

**During**

```bash
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context tasuki --namespace tasuki-dev \
  --experiment gd-01-redis-loss --print-path)"
scripts/gameday/preflight.sh --context tasuki --namespace tasuki-dev            # must say GO
scripts/gameday/watch-slis.sh --context tasuki --namespace tasuki-dev --interval 15   # second terminal
scripts/gameday/record-timeline.sh mark steady-state
scripts/gameday/record-timeline.sh snapshot                                      # SLIs into the log
scripts/gameday/redis-loss.sh --context tasuki --namespace tasuki-dev --mode delete-pod   # logs "inject" itself
scripts/gameday/record-timeline.sh note "fleet-state unready; Location pipeline shows lag rising"
# ... follow the plan's checkpoints; when an alert fires: mark detect; when the runbook step works: mark mitigate
scripts/gameday/record-timeline.sh mark recovered
scripts/gameday/record-timeline.sh snapshot
scripts/gameday/record-timeline.sh mark end
```

The context name `tasuki` is what `make kubeconfig` creates. The scripts still require it explicitly.

**After (the same day)**

1. Fill in the plan's **Results** from the drill log. Copy the numbers from the log, and compare every expected
   observation with what happened.
2. Decide whether a postmortem is required ([docs/postmortems/README.md](../postmortems/README.md)). If so, start it from
   [TEMPLATE.md](../postmortems/TEMPLATE.md) within 5 working days.
3. Open an issue for every action item (label `gameday`), and fix runbooks in their own pull requests, linked from the results.
4. Commit the drill log (`docs/gameday/logs/`) with the results.

### Schedule format

Add a row here when a drill is booked; update it when it has run. Times are JST.

| ID | Experiment | Env | Window (JST) | Driver / observer / scribe | Preflight | Status | Log | Results / postmortem |
|---|---|---|---|---|---|---|---|---|
| GD-01 | Redis loss | dev | <!-- date, start–end --> | <!-- names --> | <!-- GO/NO-GO --> | Planned | <!-- link --> | <!-- link --> |

`Status` is one of `Planned`, `Done`, `Aborted` (say why in the results) or `Blocked` (name the missing piece).

**Proposed order.** Run every drill first in dev, in sessions that cost nothing in budget: GD-07, GD-01, GD-05
(single broker, so a full Kafka outage), GD-09, GD-04 (one node), GD-08 Phase A. Then prod sessions, one damaging drill
per service per session: GD-06 (orders), GD-05 with one broker of three (location path), GD-04 AZ drain, GD-02. Run
GD-03 last, because it spends the orders budget on purpose.

## From results to fixes

| Finding | Goes to |
|---|---|
| A runbook step was wrong, missing or slow | A pull request to `docs/runbooks/`, linked from the results |
| An alert did not fire, fired late, or fired for the wrong reason | `observability/alerts/` or the Sloth spec, with a `promtool` unit test that reproduces the drill |
| The system did not behave as design §10 says | An issue for the service owner. Also update §10 if the design was wrong, not the code |
| A missing piece blocked the drill | An issue naming the drill it unblocks |
| A drill needs a postmortem | `docs/postmortems/YYYY-MM-DD-<slug>.md`, from the template |

Re-run a drill after its fixes have shipped. A fix is only proven when the drill that found it passes.

## Known gaps before the first drill

These come from reading the repository while writing the plans; none of them comes from a drill. Each plan lists its
own in more detail.

- **SLOs exist only in `tasuki-prod`**, and Prometheus data does not survive `make aws-down`, so budgets only ever
  cover one session.
- **A database failover now burns orders' budget.** Orders answers `503 database_unavailable` with `Retry-After: 2`
  and stays ready for `READINESS_DB_GRACE_SECONDS` (120 s), so the availability SLI sees a failover. A full outage
  longer than about 48 s inside an hour crosses the 13.44× page threshold, while design §10 calls 1–2 minutes of
  failed creates acceptable. That tension needs a decision (below). GD-06 measures it.
- **No fault-injection switch in the services.** GD-03 uses a database privilege fault instead.
- **No AWS Fault Injection Service** role or templates, so GD-04 can drain an AZ but not impair one.
- **The brownout is still a deploy.** `TRACKING_MIN_INTERVAL` is an environment variable; in prod tracking is a Rollout,
  so turning it on goes through the gate and the canary steps, which is slower than a plain restart in the middle of a surge.
- **No Tasuki runbook covers a dispatch replica loss specifically**; GD-10 uses the consumer-lag one.

Found while planning and since addressed in `deploy/`, `services/` or `observability/` (each plan's hypothesis now
reflects the new behaviour): orders' database errors (500 → 503 with `Retry-After`) and its readiness hiding outages;
Argo Rollouts and the gate; PostgreSQL backups; explicit ExternalSecret keys; Alertmanager receivers; a namespace in
`KafkaConsumerLagHigh`; runbooks for every drill; HPAs on tracking; scenario files for the simulator. None of these
has been proven by a drill yet.

## Open questions and decisions needing an ADR

- Should a PostgreSQL failover of 1–2 minutes (acceptable in design §10) page through `OrdersAvailabilityBudgetBurn`?
  Now that the SLI sees it, a full outage over about 48 s in an hour does. Options: accept the page, raise the
  readiness grace or the burn thresholds, or let orders queue creates during a failover. And should availability also
  be measured from the client side (a synthetic probe or load balancer metrics)?
- Should `tasuki-dev` get SLO recording rules (without alerts), so dev drills can be measured the same way?
- How should the error-budget policy treat ephemeral clusters, whose "28-day" window covers one session? Options: keep
  Prometheus data across sessions (remote write or a retained volume), or judge drills by the 28-day equivalent.
- Should services expose a fault-injection switch (for example a 5xx ratio, enabled only in non-prod builds or behind a
  flag), instead of the database-level fault GD-03 uses?
- Should prod drills use AWS Fault Injection Service for real AZ impairments, with an IAM role in `infra/`?
