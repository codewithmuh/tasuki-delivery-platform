# Runbook: Redis (Valkey) lost, or fleet-state not rebuilding it

Alert: `FleetCacheUnavailable` (`severity: page`) for 5 minutes, with a `component` label ·
[rule](../../observability/alerts/fleet.yaml) · game day [GD-01](../gameday/experiments/01-redis-loss.md)

| `component` | Means |
|---|---|
| `valkey` | StatefulSet `redis` has no ready pod |
| `fleet-state` | No fleet-state replica is ready: Redis unreachable, or its rebuild from Kafka never finishes |
| `dispatch` | dispatch has been paused for 5 minutes, waiting for its state to be rebuilt |

During one incident they often fire together. Start with `valkey` if it is there.

## Symptoms

The live fleet cache is Valkey (Redis protocol): StatefulSet `redis`, pod `redis-0`, Service `redis:6379`, no
persistence (`deploy/apps/base/redis.yaml`). It is derived from Kafka, so losing it loses nothing, and recovery is
automatic ([services/fleet/README.md](../../services/fleet/README.md)):

1. The StatefulSet recreates `redis-0`, empty, usually within 30 s.
2. fleet-state's sweeper notices that `fleet:state:rebuilt_at` is gone (every 5 s), replays the last 5 minutes of
   `courier.locations` into Redis, and sets the marker again. fleet-state is unready while it rebuilds.
3. dispatch notices that its `dispatch:sentinel` key is gone, pauses (`dispatch_paused` = 1, readiness 503), waits
   for `fleet:state:rebuilt_at` to be newer than the loss, replays the last 30 minutes (`RECOVERY_WINDOW`) of
   `orders.events` and `dispatch.events`, then resumes.

GD-01 expects all of this within about 90 seconds; the local run measured 2–5 s from Valkey back to the marker and
about 12 s to dispatch resuming. The alert fires when one step is stuck for 5 minutes.

## Impact

- **Orders: no failures; quotes can be slower.** Orders does not use Redis, but every quote asks dispatch for an ETA.
  While Valkey is down, the first ETA request on each dispatch replica waits out `ETA_BUDGET` (250 ms) before the
  503; for the next 5 s dispatch answers 503 at once, and orders uses its local estimate. Before this was fixed,
  every quote paid the 250 ms (GD-01, local, 2026-10-16).
- **Tracking: none.** Tracking reads Kafka, not Redis.
- **Dispatch: no assignments** while paused. Orders confirmed during the pause wait (each one misses 60 s), and
  `GET /v1/eta` answers 503, so quotes use orders' local estimate (`ETAFallbackHigh`, `component=orders`).
- **Positions:** while Valkey is down, fleet-state retries the same batch without committing, so its lag grows and
  nothing is lost. Location freshness burns during the catch-up.
- After recovery: orders confirmed before the 30-minute window are not restored (orders cancels unassigned ones at
  15 minutes anyway), open offers are offered again, and dispatch events that were still in its Redis outbox at the
  moment of the loss are lost.

This pages although it is a cause: a paused dispatch records no assignment events, so its SLO stays quiet.

## Confirm

```promql
max(kube_statefulset_status_replicas_ready{namespace="tasuki-prod",statefulset="redis"})   # 1 = Valkey ready
max(kube_deployment_status_replicas_available{namespace="tasuki-prod",deployment="fleet-state"})
sum(increase(fleet_state_rebuilds_total{namespace="tasuki-prod"}[30m]))                    # rebuilds done
max(fleet_state_last_rebuild_duration_seconds{namespace="tasuki-prod"})
max by (pod) (dispatch_paused{namespace="tasuki-prod"})
sum(increase(dispatch_recoveries_total{namespace="tasuki-prod"}[30m]))
max by (state) (dispatch_couriers{namespace="tasuki-prod"})                                # couriers back in the geo sets?
```

```bash
kubectl -n tasuki-prod get statefulset redis
kubectl -n tasuki-prod describe pod redis-0 | sed -n '/Conditions/,$p'      # Pending? OOMKilled? probe failing?
# The cache itself (container valkey; the image ships valkey-cli)
kubectl -n tasuki-prod exec redis-0 -c valkey -- valkey-cli PING
kubectl -n tasuki-prod exec redis-0 -c valkey -- valkey-cli EXISTS fleet:state:rebuilt_at dispatch:sentinel
kubectl -n tasuki-prod exec redis-0 -c valkey -- valkey-cli ZCARD geo:couriers:all
kubectl -n tasuki-prod exec redis-0 -c valkey -- valkey-cli INFO memory | grep -E 'used_memory_human|maxmemory_human'

# What the services say (Go services log uppercase levels)
kubectl -n tasuki-prod logs deploy/fleet-state --since=15m | jq -c 'select(.msg | test("redis|rebuil|not ready"; "i"))'
kubectl -n tasuki-prod logs -l app.kubernetes.io/name=dispatch,app.kubernetes.io/component=server --since=15m \
  | jq -c 'select(.msg | test("paused|rebuilt|recovery|Redis"))'
```

Expected log lines: fleet-state `redis lost its data; rebuilding it from kafka`, then `rebuilt redis from kafka`;
dispatch `Redis lost dispatch's state; dispatch paused until it is rebuilt`, then `dispatch state rebuilt;
dispatching`. `recovery replay failed; retrying` means dispatch cannot read Kafka.

## Mitigate

1. **`redis-0` not coming back.** `describe` says why:
   - *Pending:* no node can take it. It has no volume, so any node with room will do; see
     [node-az-loss](node-az-loss.md) for Karpenter and quota.
   - *OOMKilled or full:* Valkey runs with `maxmemory 200mb` and `noeviction` (a full cache fails writes loudly
     instead of evicting positions), inside a 256 Mi limit. Raise both in `deploy/apps/base/redis.yaml` through a pull
     request; a change made with `kubectl` is reverted by Argo CD.
   - *Stuck:* `kubectl -n tasuki-prod delete pod redis-0`. It holds nothing that is not in Kafka.
2. **Valkey is back, fleet-state stays unready.** The rebuild reads `courier.locations`: if Kafka is down, follow
   [kafka-unavailable](kafka-unavailable.md). Otherwise force a fresh rebuild by restarting fleet-state, which always
   rebuilds on start (`kubectl -n tasuki-prod rollout restart deployment/fleet-state`; Argo CD does not undo a
   restart). This is also `scripts/gameday/redis-loss.sh --rollback`.
3. **fleet-state is ready, dispatch is still paused.** dispatch waits until `fleet:state:rebuilt_at` is newer than
   the moment it last saw its sentinel. If the marker exists but is older, restart fleet-state as in step 2. If the
   logs show `recovery replay failed`, dispatch cannot read `orders.events` or `dispatch.events`: check Kafka.
4. **Orders waiting from before the pause:** nothing to do by hand. dispatch replays 30 minutes of orders, and orders
   cancels anything still unassigned at 15 minutes. Tell the dispatcher on duty that assignments were paused.

## Escalate

Not recovered 15 minutes after Valkey is ready: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Record how long each step took (Valkey ready, marker back, dispatch resumed) next to GD-01's expectations, and check
the location freshness and dispatch speed budgets. A loss that recovered by itself but took longer than 90 seconds is
worth a ticket.
