# Runbook: Kafka consumer lag or a stalled consumer

Alerts · [rules](../../observability/alerts/kafka.yaml):

- `KafkaConsumerLagHigh` (`severity: page`): the `fleet-state` group has been more than 7,500 messages behind on
  `courier.locations` (about 10 s of peak GPS traffic) for 5 minutes.
- `KafkaConsumerStalled`: a group has messages waiting on a partition and has committed nothing there for
  5 minutes. `dispatch` on `orders.events` pages; `orders` on `dispatch.events` is a ticket.

Both carry `namespace`, `consumergroup` and `topic`. They select `tasuki-prod` only. Each environment runs its own
Kafka, so dev's lag never adds to prod's.

## Symptoms

| Group (topic) | What falls behind |
|---|---|
| `fleet-state` (`courier.locations`) | Courier positions in Redis get old |
| `dispatch` (`orders.events`) | Confirmed orders on the stalled partitions are never offered to a courier |
| `orders` (`dispatch.events`) | Order statuses in the orders API stop following dispatch (assigned, picked up, delivered) |

Tracking is not covered here, because it reads its topics without a consumer group. A stalled tracking replica
raises [tracking-feed-stalled](tracking-feed-stalled.md) instead.

## Impact

- **fleet-state:** dispatch matches orders against stale positions.
- **dispatch:** each of the 12 partitions carries a twelfth of the orders. A stalled partition means those orders
  wait until orders cancels them at 15 minutes.

Both page although they are causes. A consumer that has *stopped* records no SLI events, so the location freshness
and dispatch SLOs go quiet instead of burning, and the backlog is the only signal left. For `orders`, nothing is lost
(events wait 7 days) and nothing is delayed for couriers, so it is a ticket.

## Confirm

```promql
# Lag per group, and whether it is still growing
sum by (consumergroup, topic) (kafka_consumergroup_lag{namespace="tasuki-prod"})
deriv(sum(kafka_consumergroup_lag{namespace="tasuki-prod",consumergroup="fleet-state",topic="courier.locations"})[10m:1m])

# Consuming at all? Compare the group's commit rate with the topic's produce rate
sum by (consumergroup) (rate(kafka_consumergroup_current_offset{namespace="tasuki-prod"}[5m]))
sum by (topic) (rate(kafka_topic_partition_current_offset{namespace="tasuki-prod"}[5m]))

# Skew: one partition far behind (or stuck) points to one stuck consumer
kafka_consumergroup_lag{namespace="tasuki-prod",consumergroup="dispatch",topic="orders.events"}

# Members in the group (0 = nobody is consuming)
kafka_consumergroup_members{namespace="tasuki-prod"}

# What the consumers themselves see
sum(rate(fleet_state_points_total{namespace="tasuki-prod"}[5m]))
sum by (type, result) (rate(dispatch_order_events_total{namespace="tasuki-prod"}[5m]))
max(dispatch_paused{namespace="tasuki-prod"})                      # 1 = waiting for a Redis rebuild
```

```bash
# fleet-state, orders-outbox-relay and orders-dispatch-consumer are Deployments; dispatch is an Argo Rollout in prod
kubectl -n tasuki-prod get pods -l 'app.kubernetes.io/name in (fleet-state,dispatch,orders-dispatch-consumer)'
kubectl -n tasuki-prod logs deploy/fleet-state --tail=200 | jq -c 'select(.level != "INFO")'
kubectl -n tasuki-prod logs -l app.kubernetes.io/name=dispatch,app.kubernetes.io/component=server --tail=200 \
  | jq -c 'select(.msg | test("partitions|commit|skipping"; "i"))'

# Kafka's own view, from inside a broker (Strimzi: Kafka `tasuki`, pods tasuki-dual-role-<n>, container kafka).
# Not tried on a cluster yet: the plain listener's NetworkPolicy admits only the service pods, so if the tool
# times out finding the group coordinator, use the exporter metrics above instead.
kubectl -n tasuki-prod exec tasuki-dual-role-0 -c kafka -- bin/kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 --describe --group fleet-state
```

If the brokers themselves are down, follow [kafka-unavailable](kafka-unavailable.md) instead: no consumer can catch
up while they are.

## Mitigate

1. **Rebalance storm** (pods restarting or failing readiness): stop the restarts first. OOMKilled: raise the memory
   limit in `deploy/apps/overlays/prod/values/<service>.yaml`. A bad release: roll it back in git (pin the previous
   `image.tag` in that same file). Argo CD self-heals every managed object, so `kubectl rollout undo` and
   `kubectl argo rollouts undo` are reverted within seconds. Lag clears quickly once the group is stable.
2. **Too slow** (`fleet-state` lag growing, consumers healthy): add replicas through git (`replicas:` in
   `deploy/apps/overlays/prod/values/fleet-state.yaml`), up to 12 (one per partition; more sit idle). If Redis is the
   bottleneck, check Redis before adding consumers.
3. **One partition stuck** (`KafkaConsumerStalled`, others moving): the consumer that owns it is wedged. Delete
   that pod (`kubectl -n tasuki-prod delete pod <pod>`). Its partitions move to the other replicas, and processing is
   at-least-once with dedupe on `event_id`, so a replay is safe. If the same offset sticks again, it is a record the
   consumer cannot handle: look for `skipping an unusable record` (dispatch) or `event handling failed` (orders) in
   the logs, and `dispatch_records_invalid_total`.
4. **dispatch paused** (`dispatch_paused` = 1): it does not consume while it waits for a Redis rebuild. Follow
   [redis-lost](redis-lost.md).
5. **Far behind and Redis badly stale:** catching up is still right, because fleet-state also writes the location
   history. Do not reset the group's offsets to latest without recording the gap in the history.
6. **Produce rate far above normal** (a buggy courier app, or a rain surge): check fleet-ingest. It sheds load with
   `503 Retry-After` when Kafka is struggling (`fleet_ingest_shed_requests_total`); see
   [courier-locations-lost](courier-locations-lost.md) and [surge-brownout](surge-brownout.md).

## Escalate

Lag still growing, or a partition still stuck, after 30 minutes: escalate to the owner in
[CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Check the location freshness and dispatch speed budgets once the backlog is gone; a postmortem is required if one
incident burned more than 20 % of a budget ([error-budget policy](../slo/error-budget-policy.md)).
