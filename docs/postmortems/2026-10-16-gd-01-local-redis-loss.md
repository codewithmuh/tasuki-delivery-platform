# Postmortem: quotes slowed tenfold and logs flooded while the fleet cache was lost

| | |
|---|---|
| Status | In review |
| Trigger | Game day GD-01 (planned fault injection), local Compose variant |
| Date (JST) | 2026-10-16 20:36 – 20:55 |
| Environment | Local Compose stack on one laptop (`make up`), simulator scenario `normal-weekday` from 15:00 simulated time |
| Services | Valkey (fleet cache), fleet-state, dispatch, orders |
| Severity | None: local drill, no alerting stack. In production the same drill would page through `FleetCacheUnavailable` only if recovery took 5 minutes |
| Authors | Claude (drill driver and scribe), for Muhammad Rashid |
| Reviewers | Muhammad Rashid, pending. Solo project: the policy asks for a review after a day's gap |
| Drill plan and log | [GD-01 plan](../gameday/experiments/01-redis-loss.md) · logs: [first run](../gameday/logs/2026-10-16-gd-01-local-restart.log), [re-run after fixes](../gameday/logs/2026-10-16-gd-01-local-restart-rerun.log), [FLUSHALL](../gameday/logs/2026-10-16-gd-01-local-flushall.log) · SLIs: [first run](../gameday/logs/2026-10-16-gd-01-local-slis.md), [re-run](../gameday/logs/2026-10-16-gd-01-local-rerun-slis.md) |

## Summary

The live fleet cache (Valkey) was stopped on purpose for 20 seconds and restarted empty, while the simulator drove
traffic through the local stack. The recovery path worked as designed: fleet-state rebuilt the cache from Kafka in
164 ms, dispatch rebuilt its own state and resumed 11 seconds later, tracking kept streaming, and no order failed. Two
things the hypothesis did not predict: quote latency rose from about 20 ms to 202 ms at p95 (280 ms at p99), because
every quote waited the full 250 ms ETA budget for dispatch to give up on Redis; and the Redis client library wrote
289 plain-text lines to the logs during the outage, breaking the one-JSON-line contract. Both were fixed the same day
and the drill was re-run: zero plain-text lines, and ETA requests after the first failure answered in 0.03 ms. The
drill script itself had a gap (it could stop watching before dispatch had recovered), also fixed and proven by a
second variant.

## Impact

All users are simulated. About 200 orders/h, 121 couriers online, 44 open tracking pages.

| Measure | Value | Source |
|---|---|---|
| Duration of the outage | 20 s (Valkey stopped), recovery complete 34 s after injection | [first-run log](../gameday/logs/2026-10-16-gd-01-local-restart.log) |
| Quote and order requests failed | 0 of 78 in the 9-minute window | [first-run SLIs](../gameday/logs/2026-10-16-gd-01-local-slis.md) |
| Quote latency | p95 202 ms, p99 280 ms (load tests: 16–24 ms p95) | same |
| Orders lost or duplicated | 0 (orders does not use Valkey; 3 orders were created during the outage) | first-run log |
| GPS points | none lost; points ingested during the outage reached live state up to 20 s late (location freshness p99 20.0 s; 96.49 % within 2 s over the window) | first-run SLIs |
| Tracking | positions kept flowing during the outage; server-side freshness 100 % under 10 s | first-run log and SLIs |
| Dispatch | paused 11 s (no offers); the window's assignment SLI could not be separated from a supply shortage, see below | dispatch logs |

### SLOs and error budget

There is no Prometheus in the local stack, so there are no budgets to burn and no alerts to fire. The SLIs were
computed from each service's `/metrics` with the same definitions:

| SLI | Target | During the drill window | Notes |
|---|---|---|---|
| Quote and order availability | 99.9 % | 100 % | met |
| `POST /v1/orders` under 300 ms | 95 % | 100 % | met; the slowdown was on quotes, which have no latency SLO |
| Location freshness (2 s) | 99 % | 96.49 % | burned during the catch-up, as predicted |
| Tracking freshness (10 s) | 99 % | 100 % | met |
| Dispatch assignment speed (60 s) | 95 % | 7.14 % | already burning before the injection: see "What went badly" |

Policy consequences: none (no budget locally). The postmortem is required because the hypothesis was partly wrong
and the drill found defects ([postmortems README](README.md#when-one-is-required)).

## Timeline

All times JST (UTC+9), from the drill logs and the services' JSON logs.

| Time (JST) | Event | Source |
|---|---|---|
| 20:36:59 | SLI window starts; steady state: 121 couriers in the geo index, dispatch not paused | first-run SLIs, log |
| 20:38:12 | **Inject:** `docker compose stop redis` | first-run log |
| 20:38:13 | fleet-state and dispatch go unready; go-redis starts printing plain-text dial errors | fleet-state logs |
| 20:38:23 | First quote during the outage: dispatch `GET /v1/eta` answers 503 after 250 ms; orders uses its local estimate | dispatch and orders logs |
| 20:38:32 | Valkey started again, empty | first-run log |
| 20:38:33.8 | dispatch: "Redis lost dispatch's state; dispatch paused until it is rebuilt" | dispatch logs |
| 20:38:34.0 | fleet-state: "redis lost its data; rebuilding it from kafka" | fleet-state logs |
| 20:38:34.1 | fleet-state: "rebuilt redis from kafka", 9,060 records in 164 ms; marker back | fleet-state logs |
| 20:38:40 | Location freshness for the 10-second sample: 32.77 % within 2 s (the backlog draining) | first-run SLIs timeline |
| 20:38:45.7 | dispatch: "dispatch state rebuilt; dispatching" (10.9 s) | dispatch logs |
| 20:38:50 | Location freshness back to 100 % per 10-second sample | first-run SLIs timeline |
| 20:39 – 20:49 | Findings confirmed in the logs; fixes written (ETA breaker, Redis logger); fleet tests and lint pass | this document |
| 20:49:38 | dispatch and fleet-state restarted with the fixes | `docker inspect` StartedAt |
| 20:50:40 | **Re-run** of the same injection | re-run log |
| 20:50:42 | ETA 503 after 250 ms (first failure), then 20:50:44 503 after 0.03 ms (breaker open) | dispatch logs |
| 20:51:02 – 20:51:12 | dispatch paused and rebuilt (9.5 s); zero plain-text log lines | dispatch logs |
| 20:51:00 | Drill script reported recovery 21 s after injection, before dispatch had noticed the loss: **script gap found** | re-run log |
| 20:53:59 | **Variant B:** `FLUSHALL` with the fixed script; marker back at 5 s, both services proven recovered at 14 s | FLUSHALL log |

Key intervals: Valkey back to marker back, 2 s (restart) and 5 s (FLUSHALL, the sweeper interval); Valkey back to
dispatch resuming, 13 s (first run) and 12 s (re-run).

## Detection

Locally, by the drill script's one-second timeline and the services' logs; there is no alerting stack. In a cluster,
`FleetCacheUnavailable` would not fire, because every step recovered well inside its 5-minute `for`. Nothing would
have flagged the slower quotes: quotes have no latency SLO or alert, and orders' `ETAFallbackHigh` needs a sustained
fallback share. The log flood would have been visible only to someone reading the logs.

## Response

The drill followed the plan's steps with the local script (`scripts/gameday/local/redis-loss.sh --mode restart`).
No mitigation was needed: recovery is automatic. The runbook ([redis-lost](../runbooks/redis-lost.md)) describes the
recovery sequence correctly, and its expected log lines appeared in order. Its impact section said "Orders: none",
which the drill showed to be incomplete.

## Recovery

Automatic, as designed: fleet-state's sweeper found the marker missing and replayed 5 minutes of
`courier.locations`; dispatch found its sentinel missing, waited for the marker and replayed its 30-minute window.
Steady state was confirmed by location freshness returning to 100 % per 10-second sample, the geo index back at 121
couriers, and dispatch readiness.

## Root cause and contributing factors

The trigger was the injected fault. What made the impact larger than the design says:

1. **`GET /v1/eta` always tried Redis first.** While Valkey was down, every ETA request spent dispatch's whole
   `ETA_BUDGET` (250 ms) on connection attempts before answering 503, although dispatch's own readiness check already
   knew Redis was gone. Orders waits up to 300 ms for the ETA, so every quote paid about 250 ms. The design (§10)
   says "Orders: nothing" for a Redis loss; that is true for success, not for latency.
2. **go-redis's default logger writes plain text.** The library logs connection-pool failures through Go's `log`
   package to stderr, several times a second while Redis is down. Nothing in the services replaced that logger, so the
   runtime contract (one JSON line per event) did not hold in exactly the moment the logs matter.
3. **The drill script's end condition was too weak** (process, not product). It stopped watching as soon as the
   marker existed and both services were ready, which can be true for a second or two before dispatch checks its
   sentinel. The re-run ended at 21 s while dispatch paused at 22 s.

## What went well

- The recovery design held exactly: the cache is derived from Kafka, so losing it lost nothing. fleet-state rebuilt
  9,060 records in 164 ms; dispatch rebuilt its state in 9.5–10.9 s, well inside the 60 s the plan expected.
- Tracking never noticed: it reads Kafka, not Valkey, and kept streaming positions throughout.
- Orders never failed: 0 of 78 requests, and the ETA fallback worked as designed.
- Fleet-ingest kept accepting GPS throughout; points were late, never lost.
- The runbook's expected log lines matched what the services printed.

## What went badly

- Quotes slowed tenfold, which no SLO or alert would have caught.
- The logs filled with 289 unstructured lines in 20 seconds.
- The steady state chosen for the drill (`normal-weekday` at 15:00: 121 couriers spread over all 23 wards) already
  had a poor dispatch SLI before the injection, because couriers far from the store decline offers. So the drill
  could not show the plan's hypothesis 5 ("assignment speed burns only for the orders confirmed during the pause").
  The steady-state check looked at freshness and readiness, not at dispatch speed.
- The first version of the drill script could declare recovery too early.

## Where we got lucky

- The outage was short. With a longer one, the 250 ms per quote would have continued for as long as Valkey was down.
- Quote volume was low (about one every 20 s), so few customers paid the delay. At the design's 5 quotes/s, every
  quote for the whole outage would have.

## Action items

| # | Action | Type | Critical? | Owner | Due | Issue | Status |
|---|---|---|---|---|---|---|---|
| 1 | After a Redis failure, `GET /v1/eta` answers 503 at once for 5 s (`etaRedisOpenFor`), so only the first quote per replica per 5 s pays the budget | mitigate | no | Claude | 2026-10-16 | to file | **done**, verified by the re-run (0.03 ms) |
| 2 | Route go-redis's logger into the service's JSON log at debug level (`kit.RedisLogger`) in dispatch and fleet-state | prevent | no | Claude | 2026-10-16 | to file | **done**, verified by the re-run (0 plain-text lines) |
| 3 | Drill script waits for proof of recovery: fleet-state's rebuild counter and dispatch's recovery counter both increase | process | no | Claude | 2026-10-16 | to file | **done**, verified by the FLUSHALL variant |
| 4 | Re-run GD-01 locally with a dense scenario (`small`, 2 wards) whose dispatch SLI is healthy before the injection, to check hypothesis 5 | process | no | Muhammad Rashid | 2026-10-23 | to file | open |
| 5 | Add a quote latency SLI (or at least a dashboard panel and a ticket alert) so a slow ETA path is visible | detect | no | Muhammad Rashid | 2026-10-30 | to file | open |
| 6 | Extend the steady-state check of local drills with dispatch speed, so a drill does not start from a burning SLI | process | no | Muhammad Rashid | 2026-10-23 | to file | open |

Issues are "to file": this write-up was drafted before the GitHub issues existed. File them with labels
`postmortem` and `gameday`, and replace the cells with links.

## Lessons

- "Does not use Redis" is not the same as "does not notice Redis". A synchronous call into a service that depends on
  the failed component inherits its timeouts. Every synchronous dependency needs a fast way to say "I already know".
- Third-party libraries log too. The JSON-logs contract has to include them, and a game day is where that shows.
- A drill's steady state must include the SLI the hypothesis is about, or the hypothesis cannot be checked.

## Links

- Drill plan: [GD-01](../gameday/experiments/01-redis-loss.md)
- Drill logs: [first run](../gameday/logs/2026-10-16-gd-01-local-restart.log),
  [re-run](../gameday/logs/2026-10-16-gd-01-local-restart-rerun.log),
  [FLUSHALL](../gameday/logs/2026-10-16-gd-01-local-flushall.log)
- SLI reports: [first run](../gameday/logs/2026-10-16-gd-01-local-slis.md), [re-run](../gameday/logs/2026-10-16-gd-01-local-rerun-slis.md)
- Fixes: `services/fleet/internal/dispatch/api.go` (ETA breaker), `services/fleet/internal/kit/redislog.go`,
  `scripts/gameday/local/redis-loss.sh`
- Runbook used and updated: [redis-lost](../runbooks/redis-lost.md)
