# GD-01: Redis (Valkey) loss

| | |
|---|---|
| Status | **Run locally (Compose) on 2026-10-16**; cluster run planned for roadmap phase 7 |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "Redis lost"; §7.2 step 5 |
| SLOs at stake | fleet-state location freshness (99 % ≤ 2 s); tracking freshness (99 % < 10 s, should not move); dispatch assignment speed (95 % ≤ 60 s, once dispatch runs) |
| Environments | `tasuki-dev` first, then `tasuki-prod` (`FleetCacheUnavailable` selects prod) |
| Time box | 30 minutes |
| Scripts | `redis-loss.sh`, `watch-slis.sh`, `record-timeline.sh` |

The live fleet cache is Valkey (Redis protocol): StatefulSet `redis`, pod `redis-0`, Service `redis:6379`, with no
persistence and an `emptyDir` working directory (`deploy/apps/base/redis.yaml`). Deleting the pod loses every key.

## Hypothesis

When the cache loses all its data:

1. **Tracking does not notice.** It reads `courier.locations` from Kafka, never Redis, so tracking freshness stays at
   or above 99 % and fleet-stream viewers keep receiving positions throughout.
2. **Orders does not notice.** It does not use Redis. Quote and order creation success stays at steady state.
3. **fleet-state rebuilds the cache in seconds.** While Valkey is down, the consumer retries the same batch without
   committing, so lag grows and nothing is lost. Once Valkey is back, the sweeper finds `fleet:state:rebuilt_at`
   missing within `SWEEP_INTERVAL` (5 s) and replays the last `REBUILD_WINDOW` (5 minutes) of the topic. fleet-state
   is unready while it rebuilds. **Within 60 s of Valkey being back**, the marker exists again and
   `geo:couriers:all` holds about as many couriers as before.
4. **Location freshness burns briefly.** Points written during the catch-up are older than 2 s. The burn is
   proportional to the outage and stops once lag is back to steady state.
5. **Dispatch pauses, then resumes** (once dispatch runs). It waits for the marker (`dispatch_paused` = 1), rebuilds
   its own state from Kafka (`dispatch_recoveries_total` + 1), then assigns again. Assignment speed burns only for
   the orders confirmed during the pause.

## Steady state

- **Dashboards:** Tasuki location pipeline (ingest rate, consumer lag, freshness), Tasuki overview, Tasuki SLOs (prod).
- **`watch-slis.sh`:** location freshness ≥ 0.99; tracking freshness ≥ 0.99; fleet-state lag low and flat; ingest
  accepting points; pods not ready = 0. Extra queries for this drill:

  ```bash
  scripts/gameday/watch-slis.sh --context tasuki --namespace tasuki-dev --interval 10 \
    --extra 'rebuilds (10m)=sum(increase(fleet_state_rebuilds_total{namespace="{ns}"}[10m]))' \
    --extra 'last rebuild (s)=max(fleet_state_last_rebuild_duration_seconds{namespace="{ns}"})' \
    --extra 'fleet-stream events/s by pod=sum by (pod) (rate(tracking_event_age_seconds_count{namespace="{ns}",stream="fleet"}[1m]))' \
    --extra 'dispatch paused=max(dispatch_paused{namespace="{ns}"})'
  ```

- **The cache itself:** `kubectl --context tasuki -n tasuki-dev exec redis-0 -c valkey -- valkey-cli ZCARD geo:couriers:all`
  (couriers seen in the last 60 s) and `... valkey-cli EXISTS fleet:state:rebuilt_at` (1). `redis-loss.sh` prints both.

## Blast radius

- One namespace's Valkey. Only fleet-state (writes) and dispatch (reads, and its own state) use it.
- Budget: location freshness, and dispatch assignment speed once dispatch runs. Orders and tracking budgets should
  not move; if they do, the hypothesis is wrong.
- No data loss is possible: the cache is derived from `courier.locations` (24 h retention).

## Preconditions

- Preflight GO.
- The simulator has run for **at least 15 minutes**, so the 5-minute rebuild window has data. Start it with a pull
  request to `deploy/apps/overlays/<env>/values/simulator.yaml`:

  ```yaml
  replicas: 1
  env:
    SCENARIO: small   # dev at any hour: fixed 11:00 start. Prod: normal-weekday at lunch or dinner (JST)
  ```

- Couriers are reporting: `fleet_ingest_points_total{result="accepted"}` is rising.
- **A fleet-stream viewer is connected.** The simulator's customers open order tracking streams
  (`demand.watch_share`), which already show whether tracking keeps sending during the drill, but hypothesis 1 is
  about the fleet stream too, which only the ops console opens. Open one, without printing the token:

  ```bash
  kubectl --context tasuki -n tasuki-dev port-forward svc/tracking 8083:80 &
  FLEET_STREAM_TOKEN="$(kubectl --context tasuki -n tasuki-dev get secret tasuki-app \
    -o jsonpath='{.data.FLEET_STREAM_TOKEN}' | base64 -d)"
  curl -sN -H "Authorization: Bearer ${FLEET_STREAM_TOKEN}" http://127.0.0.1:8083/v1/fleet/stream \
    | grep --line-buffered '^event:' | uniq -c
  ```

## Steps

```bash
CTX=tasuki NS=tasuki-dev          # prod run: NS=tasuki-prod and export ALLOW_PROD=yes
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context $CTX --namespace $NS \
  --experiment gd-01-redis-loss --print-path)"
scripts/gameday/preflight.sh --context $CTX --namespace $NS --service fleet-state
scripts/gameday/record-timeline.sh mark steady-state
scripts/gameday/record-timeline.sh snapshot
```

1. **Inject (variant A, the pod is lost):**
   `scripts/gameday/redis-loss.sh --context $CTX --namespace $NS --mode delete-pod`.
   The script deletes `redis-0`, waits for the replacement to be Ready, then polls for `fleet:state:rebuilt_at`
   and prints how long each step took. Those numbers go into the drill log automatically.
2. **Watch fleet-state:**
   `kubectl --context $CTX -n $NS get pods -l app.kubernetes.io/name=fleet-state -w` (expect unready, then ready) and
   `kubectl --context $CTX -n $NS logs deploy/fleet-state --since=5m | jq -c 'select(.msg | test("rebuild|redis"; "i"))'`
   (expect "redis lost its data; rebuilding it from kafka").
3. **Checkpoints** (note each with `record-timeline.sh note`):
   - T+30 s: `redis-0` Running and Ready.
   - T+60 s after Valkey is Ready: the marker exists, the courier count is back near its steady-state value, and
     fleet-state is Ready.
   - Throughout: the fleet-stream viewer keeps printing `event: courier` lines, and tracking's events/s per pod stays flat.
4. Wait until location freshness (5 m) is back at its steady-state value, then `record-timeline.sh mark recovered`
   and `snapshot`.
5. **Variant B (the data is lost, the process stays up)**, after at least 10 minutes of steady state:
   `redis-loss.sh ... --mode flushall`. This tests only the marker check, without a pod restart or reconnect.
6. `record-timeline.sh mark end`.

## Expected observations

| Signal | Expected |
|---|---|
| `redis-0` | Replaced within about 30 s (no volume to attach) |
| fleet-state | Unready during the outage and the rebuild; `fleet_state_rebuilds_total` + 1; `fleet_state_last_rebuild_duration_seconds` a few seconds |
| fleet-state consumer lag | Rises while Valkey is down, drains after; stays far below 7,500 at this scale |
| Location freshness (5 m) | Dips during catch-up, recovers within minutes |
| Tracking freshness, fleet-stream events/s | Unchanged |
| Orders availability, simulator order failures | Unchanged |
| Dispatch (once it runs) | `dispatch_paused` 1 then 0; `dispatch_recoveries_total` + 1; assignments resume |

## Abort criteria

In addition to the [global ones](../README.md#abort-criteria-all-experiments):

- The marker is still missing 5 minutes after Valkey is Ready (the rebuild is stuck).
- Tracking or orders degrade. The drill has found a coupling the design says does not exist: stop and record it.

## Rollback

Nothing to restore: the cache is derived. If the marker does not come back, follow
[redis-lost](../../runbooks/redis-lost.md), or
`redis-loss.sh --context $CTX --namespace $NS --rollback` restarts fleet-state, which always rebuilds on start, and waits
for it to be Ready.

## Alerts and runbooks

| Alert | Should it fire? | Runbook |
|---|---|---|
| `FleetStateLocationFreshnessBudgetBurn` (prod) | Only if the outage lasts minutes. A 30-second loss should stay under the page thresholds (1 h and 5 m windows both above 13.44× burn) | [slo-fleet-state-location-freshness](../../runbooks/slo-fleet-state-location-freshness.md), step 3 "Redis slow or lost" |
| `FleetCacheUnavailable` (`component` valkey, fleet-state or dispatch) | **No**, unless recovery is stuck: it needs Valkey not ready, no fleet-state replica ready, or dispatch paused, for 5 minutes. If it fires, the rebuild did not finish in time, and that is the finding | [redis-lost](../../runbooks/redis-lost.md) |
| `KubeStatefulSetReplicasMismatch` (upstream) | No: Valkey is back long before its 15 minutes | [redis-lost](../../runbooks/redis-lost.md) |
| `KafkaConsumerLagHigh` | No (7,500 messages for 5 minutes is far above this scale) | [kafka-consumer-lag](../../runbooks/kafka-consumer-lag.md) |
| `TrackingFeedStalled` | **No.** If it fires, tracking depends on Redis after all | [tracking-feed-stalled](../../runbooks/tracking-feed-stalled.md) |
| `DispatchAssignmentSpeedBudgetBurn` (prod, once dispatch runs) | Not for a short loss | [slo-dispatch-assignment-speed](../../runbooks/slo-dispatch-assignment-speed.md) |

**Follow [redis-lost](../../runbooks/redis-lost.md) during the drill** as the on-call would, and note every step that
is wrong, missing or slow.

## Gaps found while planning

- None open. The two found earlier (no alert for an empty cache or an unready fleet-state; `KafkaConsumerLagHigh`
  summing both environments) are addressed by `FleetCacheUnavailable` and a namespace in the lag rule. The drill is
  what proves them.

## Results

### Local (Compose), 2026-10-16

| | |
|---|---|
| Date and window (JST) | 2026-10-16 20:36–20:55 |
| Environment, variant | Local Compose stack on one laptop; variant A (`restart`, Valkey down 20 s) twice, before and after fixes; variant B (`flushall`) once |
| Driver / observer / scribe | Claude, for Muhammad Rashid |
| Drill log | [first run](../logs/2026-10-16-gd-01-local-restart.log), [re-run](../logs/2026-10-16-gd-01-local-restart-rerun.log), [FLUSHALL](../logs/2026-10-16-gd-01-local-flushall.log); SLIs: [first run](../logs/2026-10-16-gd-01-local-slis.md), [re-run](../logs/2026-10-16-gd-01-local-rerun-slis.md) |
| Hypothesis held? | 1 yes · 2 partly (no order failed, but quotes slowed) · 3 yes · 4 yes · 5 not checkable (see surprises) |

| Signal | Expected | Observed |
|---|---|---|
| Valkey back (s after inject) | ≈ 30 (pod) | 21 (container restart after a 20 s stop); 1 for FLUSHALL |
| Marker back (s after Valkey back) | ≤ 60 | 2 (restart), 5 (FLUSHALL: the sweeper interval) |
| fleet-state rebuild | a few seconds | 9,060 records in 164 ms |
| dispatch | pauses, rebuilds, resumes | paused, resumed 10.9 s and 9.5 s after noticing the loss |
| Couriers in `geo:couriers:all`, before / after | about equal | 121 / 121 |
| Location freshness | brief dip | 32.77 % within 2 s for the 10 s after Valkey came back, then 100 %; 96.49 % over the 9-minute window |
| Tracking freshness | unchanged | 100 % under 10 s; events kept flowing during the outage |
| Orders | unchanged | 0 of 78 requests failed; **quotes p95 202 ms, p99 280 ms** (normally about 20 ms) |
| Alerts that fired | none expected for a short loss | none (no alerting stack locally) |
| Budget remaining before / after (prod) | | not applicable locally |

Surprises:

1. Every quote during the outage waited dispatch's whole 250 ms ETA budget, because `GET /v1/eta` tried Redis
   first every time. Fixed: after a Redis failure it answers 503 at once for 5 s (re-run: 0.03 ms after the first).
2. go-redis printed 289 plain-text lines in 20 s, breaking the JSON-logs contract. Fixed: its logger now writes JSON
   at debug level (re-run: 0 lines).
3. The drill script could declare recovery before dispatch noticed the loss. Fixed: it now waits for fleet-state's
   rebuild counter and dispatch's recovery counter to rise (proven by the FLUSHALL variant).
4. The steady state (`normal-weekday` at 15:00, 121 couriers over 23 wards) already had a poor dispatch SLI before
   the injection, so hypothesis 5 could not be checked. Re-run with a dense scenario.

Action items: see the postmortem.

Postmortem: [2026-10-16-gd-01-local-redis-loss](../../postmortems/2026-10-16-gd-01-local-redis-loss.md) (required:
hypothesis 2 was partly wrong and the drill found defects).

### Cluster (`tasuki-dev`, then `tasuki-prod`)

Not run yet: the cluster is roadmap phase 7. Record it in the same shape as the local results above, adding the
alerts that fired and the budget before and after.
