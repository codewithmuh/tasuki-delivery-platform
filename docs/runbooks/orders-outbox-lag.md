# Runbook: orders outbox lag

Alert: `OrdersOutboxLagHigh` (`severity: page`): the oldest unpublished outbox row has been older than 60 s for
5 minutes · [rule](../../observability/alerts/orders.yaml)

## Symptoms

Confirmed orders sit in the outbox table (`messaging_outboxevent`, rows with `published_at` null) instead of
reaching Kafka (`orders.events`). Dispatch does not see them, so no courier is offered the order. The relay is its
own Deployment, `orders-outbox-relay` (one replica in prod), running `python manage.py relay_outbox` from the orders
image.

## Impact

Orders are accepted and safe in PostgreSQL. Nothing is lost, and the relay publishes everything once it recovers. But
every minute of lag is a minute without a courier. This alert pages although it is a cause: the dispatch SLI only
records an order when dispatching ends, so the dispatch SLO cannot see a stalled relay in time.

## Confirm

```promql
max(orders_outbox_lag_seconds{namespace="tasuki-prod"})                       # age of the oldest unpublished event
max(orders_outbox_pending_events{namespace="tasuki-prod"})                    # how many are waiting
sum(rate(orders_outbox_publish_failures_total{namespace="tasuki-prod"}[5m]))  # Kafka refusing acknowledgements?
sum(rate(orders_outbox_published_total{namespace="tasuki-prod"}[5m]))         # draining at all?
```

```bash
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=orders-outbox-relay   # running and ready?
# orders logs are JSON with lowercase levels; the relay names the event Kafka refused
kubectl -n tasuki-prod logs deploy/orders-outbox-relay --tail=300 \
  | jq -c 'select(.level == "error" or .level == "warning")'                    # "kafka delivery failed" has event_id
```

Backlog size and age, read from the database. The orders image has no `psql` (so `manage.py dbshell` fails); the
PostgreSQL instance image has it. With the CloudNativePG kubectl plugin, which connects to the primary:

```bash
kubectl cnpg psql orders-db -n tasuki-prod -- -d orders -c \
  "select count(*), min(created_at), now() - min(created_at) as oldest
     from messaging_outboxevent where published_at is null;"
```

Without the plugin, exec into the primary's `postgres` container:

```bash
primary="$(kubectl -n tasuki-prod get cluster orders-db -o jsonpath='{.status.currentPrimary}')"
kubectl -n tasuki-prod exec "$primary" -c postgres -- psql -d orders -c \
  "select topic, count(*), min(created_at) from messaging_outboxevent where published_at is null group by topic;"
```

Then check Kafka itself: are the brokers up, and is `orders.events` writable? Kafka runs in the same namespace
(Strimzi, Kafka `tasuki`, pods `tasuki-dual-role-<n>`):

```bash
kubectl -n tasuki-prod get kafka tasuki
kubectl -n tasuki-prod get pods -l strimzi.io/cluster=tasuki,strimzi.io/pool-name=dual-role
```

If `KafkaUnavailable` is firing too, follow [kafka-unavailable](kafka-unavailable.md).

## Mitigate

1. **Kafka down:** this is designed degradation (design §10). Orders keep coming in, and the outbox drains when Kafka
   is back. Fix Kafka.
2. **Relay stuck or crash-looping:** restart it (`kubectl -n tasuki-prod rollout restart deployment/orders-outbox-relay`,
   which Argo CD does not undo, or delete the pod). The relay is at-least-once and consumers dedupe on `event_id`, so
   a restart that republishes a few rows is safe.
3. **One row fails every time (poison message):** the logs show `kafka delivery failed` with the same `event_id`
   again and again. Record the row in the incident, move it aside so the rest can drain, and replay it once it is
   fixed. Never delete it silently: it is an order that dispatch has not seen.
4. **Database trouble:** the relay reads and updates the outbox in PostgreSQL. If `OrdersDatabaseUnavailable` is
   firing, follow [postgres-failover](postgres-failover.md). A long-running transaction or lock on
   `messaging_outboxevent` also stalls it; find it with
   `select pid, state, now() - xact_start, query from pg_stat_activity where state <> 'idle' order by 3 desc nulls last;`
   and end it (`select pg_terminate_backend(<pid>);`) once you know what it is.

## Escalate

Not draining within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Check how much of the dispatch SLO budget this burned once the backlog cleared. A postmortem is required above 20 %.
