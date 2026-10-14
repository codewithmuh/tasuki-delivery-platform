# Runbook: orders outbox lag

Alert: `OrdersOutboxLagHigh` (`severity: page`): the oldest unpublished outbox row has been older than 60 s for
5 minutes · [rule](../../observability/alerts/orders.yaml)

## Symptoms

Confirmed orders sit in the `outbox` table instead of reaching Kafka (`orders.events`). Dispatch does not see them, so
no courier is offered the order.

## Impact

Orders are accepted and safe in PostgreSQL. Nothing is lost, and the relay publishes everything once it recovers. But
every minute of lag is a minute without a courier. This alert pages although it is a cause: the dispatch SLI only
records an order when dispatching ends, so the dispatch SLO cannot see a stalled relay in time.

## Confirm

```promql
max(orders_outbox_lag_seconds{namespace="tasuki-prod"})                   # age of the oldest unpublished event
max(orders_outbox_pending_events{namespace="tasuki-prod"})                # how many are waiting
sum(rate(orders_outbox_publish_failures_total{namespace="tasuki-prod"}[5m]))  # Kafka refusing acknowledgements?
sum(rate(orders_outbox_published_total{namespace="tasuki-prod"}[5m]))     # draining at all?
```

```bash
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=orders          # is the relay running?
kubectl -n tasuki-prod logs deploy/orders --tail=300 | jq -c 'select(.level == "ERROR")'
# Backlog size and age (table and column names per the orders migrations)
kubectl -n tasuki-prod exec deploy/orders -- python manage.py dbshell -- -c \
  "select count(*), min(created_at) from outbox where published_at is null;"
```

Then check Kafka itself: are the brokers up, and is `orders.events` writable? (`kubectl -n kafka get kafka,pods`)

## Mitigate

1. **Kafka down:** this is designed degradation (design §10). Orders keep coming in, and the outbox drains when Kafka
   is back. Fix Kafka.
2. **Relay stuck or crash-looping:** restart it. The relay is at-least-once and consumers dedupe on `event_id`, so a
   restart that republishes a few rows is safe.
3. **One row fails every time (poison message):** the logs show the same `event_id` again and again. Record the row in
   the incident, move it aside so the rest can drain, and replay it once it is fixed. Never delete it silently: it is
   an order that dispatch has not seen.
4. **Long-running transaction or lock** on `outbox`: find and end it (`pg_stat_activity`).

## Escalate

Not draining within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Check how much of the dispatch SLO budget this burned once the backlog cleared. A postmortem is required above 20 %.
