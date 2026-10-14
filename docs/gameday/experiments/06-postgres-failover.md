# GD-06: PostgreSQL primary failover

| | |
|---|---|
| Status | **Planned, not run** |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "PostgreSQL primary fails"; §7.1 (idempotency) |
| SLOs at stake | orders availability and latency |
| Environment | `tasuki-prod` (CloudNativePG `orders-db`: primary + asynchronous standby in another AZ). In dev (one instance) the same script is a restart drill |
| Time box | 30 minutes |
| Scripts | `pg-failover.sh`, `watch-slis.sh`, `record-timeline.sh` |

## Hypothesis

When the `orders-db` primary goes away:

1. **CloudNativePG promotes the standby** and points `orders-db-rw` at it. Orders creation fails for **1–2 minutes at
   most** (design §10), usually less: a planned switchover should take seconds, an unplanned failover longer (the
   operator first has to decide the primary is gone).
2. **Retries are safe.** The simulator retries a failed create up to 4 times **with the same `Idempotency-Key` and
   body**, honouring `Retry-After`. Every order is created exactly once: a retry whose first attempt had committed
   gets the original response (`Idempotent-Replayed: true`, `simulator_orders_replayed_total`). No duplicate orders, no
   lost confirmed orders.
3. **Clients get `503 database_unavailable` with `Retry-After: 2`** while the primary is away (orders README,
   "Database outages"), counted in `orders_database_unavailable_total{reason}` (`connection`, `shutdown`, `read_only`, ...).
   Nothing is half-written: each write is one transaction, rolled back before the answer.
4. **The SLI sees the failover, and that may page.** Orders' pods stay ready for `READINESS_DB_GRACE_SECONDS` (120 s)
   after the first failed database check, so requests keep reaching Django and every failed one is a 503 in
   `http_requests_total`. That spends the availability budget, as it should. But the page threshold is a 1.34 %
   error ratio over an hour: **a full outage of creates and quotes longer than about 48 s in an hour may page
   `OrdersAvailabilityBudgetBurn`**, although design §10 calls 1–2 minutes acceptable. A switchover should stay well
   under that; an unplanned failover may not. Whichever happens is the finding (an ADR question, see the README).
   If the outage outlasts the 120 s grace, pods go unready and the SLI goes quiet again, as before.
5. **Asynchronous replication can lose the last transactions** on an unplanned failover (deploy/README.md). An order
   confirmed to a client in the last moment before the crash could disappear. The order counts after the drill tell.

## Steady state

- **Dashboards:** Tasuki orders & outbox, Tasuki service RED (orders), Tasuki SLOs, CloudNativePG's dashboard.
- **`watch-slis.sh`** (its "client view" line is the simulator's quote and order failure ratio) with:

  ```bash
  --extra 'sim failures by reason=sum by (stage, reason) (rate(simulator_orders_failed_total{namespace="{ns}"}[1m]))' \
  --extra 'idempotent replays/min=sum(rate(simulator_orders_replayed_total{namespace="{ns}"}[5m])) * 60' \
  --extra 'orders ready pods=sum(kube_pod_status_ready{namespace="{ns}",condition="true",pod=~"orders-[a-z0-9]+-[a-z0-9]+"})' \
  --extra 'pg replication lag (s)=max(cnpg_pg_replication_lag{namespace="{ns}"})' \
  --extra 'orders 503 by reason=sum by (reason) (rate(orders_database_unavailable_total{namespace="{ns}"}[1m]))'
  ```

- `kubectl --context tasuki -n tasuki-prod get cluster orders-db` (`Cluster in healthy state`, 2/2 ready) and, with the
  plugin, `kubectl cnpg status orders-db -n tasuki-prod`.

## Blast radius

Orders in `tasuki-prod`: quotes and creates fail for the failover window; the outbox relay and the dispatch consumer
reconnect. Location path unaffected. Budget: orders availability, which now counts the failover (hypothesis 4).

## Preconditions

- Preflight GO (`--service orders`, above 50 %), and no other drill on orders this session.
- `orders-db` reports 2 instances ready, and replication lag near 0.
- Simulator on `normal-weekday` at a meal peak, so creates are frequent.
- For `--mode switchover` and the rollback: the CloudNativePG kubectl plugin (`kubectl cnpg`).

## Steps

```bash
CTX=tasuki NS=tasuki-prod; export ALLOW_PROD=yes
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context $CTX --namespace $NS \
  --experiment gd-06-postgres-failover --print-path)"
scripts/gameday/preflight.sh --context $CTX --namespace $NS --service orders
scripts/gameday/record-timeline.sh mark steady-state; scripts/gameday/record-timeline.sh snapshot
```

1. **Run 1, planned switchover:** `scripts/gameday/pg-failover.sh --context $CTX --namespace $NS --mode switchover`.
   The script prints each change of `status.currentPrimary` and `status.phase` with the elapsed seconds.
2. Wait for steady state (10 minutes). `snapshot`.
3. **Run 2, unplanned failover:** `pg-failover.sh ... --mode kill-primary` (deletes the primary pod with a 1-second
   grace period).
4. **Checkpoints** during each run:
   - `kubectl -n $NS get pods -l app.kubernetes.io/name=orders -w`: the pods should stay ready (grace 120 s).
   - Simulator failures by reason: `http_503`, retried after 2 s; `network` would mean pods went unready.
   - `orders 503 by reason` in `watch-slis.sh`, and `kubectl -n $NS logs deploy/orders --since=5m | jq -c 'select(.status >= 500)'`.
   - Whether `OrdersAvailabilityBudgetBurn` pages, and how long creates were failing (hypothesis 4).
5. **After each run, the exactly-once check.** `Order.quote` is unique, so the database itself rules out two orders
   for one quote; the real check is the counts. Compare the growth of `simulator_orders_created_total` (what clients
   were told) with the rows the database kept for the same window:

   ```sql
   -- on the new primary: kubectl -n tasuki-prod exec -it <primary> -c postgres -- psql -d orders
   select count(*) from orders_order where created_at >= '<inject time - 5 min>' and created_at < '<recovered + 5 min>';
   ```

   Fewer rows than the simulator counted means confirmed orders were lost (hypothesis 5).

6. `pg-failover.sh ... --rollback` switches back to the original primary (needs the plugin). `mark recovered`, `snapshot`, `mark end`.

## Expected observations

| Signal | Switchover | Unplanned failover |
|---|---|---|
| Time to a new primary | Seconds | Tens of seconds up to ~2 minutes |
| Orders pods | Stay ready | Stay ready (unless the outage outlasts 120 s) |
| What clients get | A few `503 database_unavailable`, retried successfully | More 503s; some orders may exhaust 4 retries |
| Server-side 5xx in `http_requests_total` | Few, all 503 | The whole outage, all 503 |
| `OrdersAvailabilityBudgetBurn` | No | Possibly, if creates fail for more than about 48 s |
| Duplicate orders | 0 | 0 |
| Lost confirmed orders | 0 | Possibly a few (asynchronous replication): record them |

## Abort criteria

In addition to the [global ones](../README.md#abort-criteria-all-experiments):

- No primary after 5 minutes (`status.currentPrimary` empty, or the phase not healthy).
- Orders still failing 3 minutes after the new primary is ready.
- Any duplicate order.

## Rollback

`pg-failover.sh --rollback` switches back to the original primary and waits for a healthy cluster. If the old primary
does not rejoin as a standby, CloudNativePG rebuilds it from the new primary; follow `kubectl cnpg status`.

## Alerts and runbooks

Follow [postgres-failover](../../runbooks/postgres-failover.md) during each run, and note every step that is wrong or slow.

| Alert | Should it fire? | Runbook |
|---|---|---|
| `OrdersDatabaseFailover` (ticket) | **Yes, after each run**: a standby became the primary | [postgres-failover](../../runbooks/postgres-failover.md) |
| `OrdersDatabaseUnavailable` (page) | Only if no instance accepts writes for 2 minutes | [postgres-failover](../../runbooks/postgres-failover.md) |
| `OrdersDatabaseReplicationDegraded` (ticket) | Only if the old primary has not rejoined as a standby after 10 minutes | [postgres-failover](../../runbooks/postgres-failover.md) |
| `OrdersAvailabilityBudgetBurn` (page) | Not for a switchover; **possibly** for an unplanned failover over about 48 s (hypothesis 4) | [slo-orders-availability](../../runbooks/slo-orders-availability.md) |
| `OrdersOutboxLagHigh` | No (the relay reconnects within seconds) | [orders-outbox-lag](../../runbooks/orders-outbox-lag.md) |

## Gaps found while planning

- **A failover now pages, maybe.** The 503 and the readiness grace (both added after planning found 500s and a
  readiness check that hid outages from the SLI) make the SLI honest, and expose a tension: design §10 accepts 1–2
  minutes of failed creates, while the burn alert pages at about 48 s of full outage in an hour. Needs an ADR.
- A real merchant client might not honour `Retry-After`; the simulator does.

## Results

<!-- fill in after the drill -->

| | |
|---|---|
| Date and window (JST) | <!-- fill in after the drill --> |
| Runs (switchover, kill-primary) | <!-- fill in after the drill --> |
| Driver / observer / scribe | <!-- fill in after the drill --> |
| Drill log | <!-- link --> |
| Hypothesis held? | <!-- per numbered point --> |

| Signal | Expected | Observed (switchover) | Observed (failover) |
|---|---|---|---|
| Time to new primary (s) | s / ≤ 120 | <!-- fill in after the drill --> | <!-- fill in after the drill --> |
| Orders creation failing, client view (s) | ≤ 120 | <!-- fill in after the drill --> | <!-- fill in after the drill --> |
| HTTP status clients saw | 503 + `Retry-After: 2` | <!-- fill in after the drill --> | <!-- fill in after the drill --> |
| Orders pods unready? | no (120 s grace) | <!-- fill in after the drill --> | <!-- fill in after the drill --> |
| Orders that exhausted retries | few | <!-- fill in after the drill --> | <!-- fill in after the drill --> |
| Duplicate orders | 0 | <!-- fill in after the drill --> | <!-- fill in after the drill --> |
| Confirmed orders lost | 0 / few | <!-- fill in after the drill --> | <!-- fill in after the drill --> |
| Budget remaining before / after | | <!-- fill in after the drill --> | <!-- fill in after the drill --> |
| Alerts that fired (expect `OrdersDatabaseFailover`) | | <!-- fill in after the drill --> | <!-- fill in after the drill --> |

Surprises: <!-- fill in after the drill -->

Action items: <!-- fill in after the drill -->

Postmortem: <!-- link, or "not required" with the reason -->
