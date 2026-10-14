# Runbook: dispatch assignment speed budget burn

Alert: `DispatchAssignmentSpeedBudgetBurn` (`severity: page` or `ticket`) · SLO: 95 % of on-demand orders with an
accepted courier within 60 s of confirmation, over 28 days · [spec](../../observability/slo/specs/dispatch.yaml)

## Symptoms

- **page:** more than 67 % of orders took over 60 s to get a courier (or got none) over the last hour and 5 minutes,
  or more than 28 % over 6 hours and 30 minutes.
- **ticket:** a slower burn over a day or 3 days.

## Impact

Food waits at the store and ETAs slip. The clock starts at `OrderConfirmed`, so a slow outbox or Kafka counts too.

## How P1 dispatch works (what can break)

Greedy matching ([services/fleet/README.md](../../services/fleet/README.md), "dispatch"):

- **Ownership by Kafka partition.** Replicas share `orders.events` through the consumer group `dispatch`. The topic
  is keyed by order, so the replica that owns a partition offers that partition's orders. When a replica dies, its
  partitions move to the others in a group rebalance. There are no leases or fencing in P1 (those arrive in P3).
- **All state in Redis** (`dispatch:*` keys), changed by atomic Lua scripts. A courier is reserved with `SET NX`
  for the offer's lifetime, so two replicas can never offer the same courier at once.
- **Matching:** every second (and at once for a new order), oldest order first: free couriers near the store from
  fleet-state's geo sets, ranked by OSRM road time, offered one at a time. An offer lasts `OFFER_TTL` (30 s);
  decline or expiry moves on to the next courier.
- **Offers reach couriers** through one Redis pub/sub channel (`dispatch:offers`) that every replica subscribes to,
  plus a re-read of the open offer on every 15 s heartbeat of the courier's stream.
- **dispatch.events** go through a Redis outbox and are resent after `OUTBOX_LEASE` (10 s) if Kafka has not
  acknowledged them.
- **No courier:** after 3 offers, or 60 s after confirmation with no offer made, dispatch emits `NoCourierAvailable`
  (that order is now bad for the SLI), repeats it at most every 60 s while it keeps searching, and orders cancels the
  order at 15 minutes.
- **Redis lost:** dispatch pauses (readiness 503, `dispatch_paused` = 1) until fleet-state's `fleet:state:rebuilt_at`
  is newer than the loss, then replays the last `RECOVERY_WINDOW` (30 min) of `orders.events` and `dispatch.events`.
  Orders confirmed before that window are not restored, and open offers are offered again.

## Confirm

First decide: **is it software, or is it supply?** In a rain surge, demand can exceed courier capacity (design §4).
Then the SLO burns even with healthy software, and the fix is honest quoting, not code.

```promql
# Assignment time p50 / p95 for orders that got a courier
histogram_quantile(0.5,  sum by (le) (rate(dispatch_assignment_duration_seconds_bucket{namespace="tasuki-prod",outcome="assigned"}[5m])))
histogram_quantile(0.95, sum by (le) (rate(dispatch_assignment_duration_seconds_bucket{namespace="tasuki-prod",outcome="assigned"}[5m])))

# Share of orders that ended with no courier (always bad for the SLO): high means supply, not speed
sum(rate(dispatch_assignment_duration_seconds_count{namespace="tasuki-prod",outcome="no_courier"}[5m]))
/ sum(rate(dispatch_assignment_duration_seconds_count{namespace="tasuki-prod"}[5m]))

# Supply: free and online couriers (one global value; use max), and orders waiting per ward
max by (state) (dispatch_couriers{namespace="tasuki-prod"})
sort_desc(sum by (ward) (dispatch_pending_orders{namespace="tasuki-prod"}))

# Offers: are couriers declining or letting them expire?
sum by (outcome) (rate(dispatch_offers_total{namespace="tasuki-prod"}[10m]))

# Is dispatch working at all? Paused for a Redis rebuild, or not consuming orders.events
max(dispatch_paused{namespace="tasuki-prod"})
sum by (partition) (kafka_consumergroup_lag{namespace="tasuki-prod",consumergroup="dispatch",topic="orders.events"})

# Are orders reaching dispatch? Outbox lag (OrdersOutboxLagHigh pages above 60 s)
max(orders_outbox_lag_seconds{namespace="tasuki-prod"})

# Road times: is dispatch on the straight-line fallback?
sum(rate(dispatch_eta_requests_total{namespace="tasuki-prod",source="fallback"}[5m]))
/ sum(rate(dispatch_eta_requests_total{namespace="tasuki-prod"}[5m]))
```

```bash
kubectl argo rollouts get rollout dispatch -n tasuki-prod       # dispatch is an Argo Rollout in prod
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=dispatch,app.kubernetes.io/component=server
kubectl -n tasuki-prod logs -l app.kubernetes.io/name=dispatch,app.kubernetes.io/component=server --tail=300 \
  | jq -c 'select(.msg | test("NoCourierAvailable|offer expired|paused|partitions"; "i"))'
```

## Mitigate

1. **Recent rollout?** During a canary: `kubectl argo rollouts abort dispatch -n tasuki-prod`. After one: pin the
   previous `image.tag` in `deploy/apps/overlays/prod/values/dispatch.yaml` and merge (Argo CD self-heal reverts
   `kubectl argo rollouts undo`; the `rollbackWindow` skips the canary steps).
2. **Outbox lag:** follow [orders-outbox-lag](orders-outbox-lag.md). Dispatch cannot assign orders it has not seen.
3. **dispatch paused, or a partition not consumed** (`FleetCacheUnavailable` with `component=dispatch`,
   `KafkaConsumerStalled`): follow [redis-lost](redis-lost.md) or [kafka-consumer-lag](kafka-consumer-lag.md).
4. **A dispatch replica died:** its partitions move to the surviving replica in a rebalance (seconds), and the new
   owner reads the same pending orders from Redis. Nothing to do unless the survivor is overloaded; if partitions
   stay unconsumed, `KafkaConsumerStalled` fires.
5. **OSRM down:** dispatch falls back to straight-line ETAs and keeps assigning. See [eta-fallback](eta-fallback.md).
6. **Couriers missing** (`dispatch_couriers{state="online"}` far below the shift plan, `CourierLocationsLost` or
   `CourierTokensRejected` firing): couriers cannot report or take offers. Follow
   [courier-locations-lost](courier-locations-lost.md).
7. **Supply, not software:** confirm that quotes show longer ETAs (design §4; P1 has no surge fee yet). Tell the
   dispatcher on duty, and see [surge-brownout](surge-brownout.md). This burn is real and stays in the budget.

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS). For supply,
escalate to the dispatcher on duty.

## Afterwards

A postmortem is required if one incident burned more than 20 % of the budget
([error-budget policy](../slo/error-budget-policy.md)). If the cause was supply, the action items go to the surge
model, not the code.
