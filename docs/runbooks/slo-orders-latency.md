# Runbook: orders latency budget burn

Alert: `OrdersLatencyBudgetBurn` (`severity: page` or `ticket`) · SLO: 95 % of `POST /v1/orders` under 300 ms over
28 days · [spec](../../observability/slo/specs/orders.yaml)

## Symptoms

- **page:** more than 67 % of order creates took 300 ms or longer over the last hour and the last 5 minutes (13.44×
  burn), or more than 28 % over 6 hours and 30 minutes (5.6×).
- **ticket:** a slower burn (more than 14 % over a day, or more than 4.7 % over 3 days).

## Impact

Merchant checkouts wait on us. Slow creates also hold database connections longer, which can turn into 5xx (see
[slo-orders-availability](slo-orders-availability.md)).

## Confirm

```promql
# Share of creates under 300 ms right now (the SLI; the SLO wants ≥ 0.95)
sum(rate(http_request_duration_seconds_bucket{namespace="tasuki-prod",service="orders",method="POST",route="/v1/orders",le="0.3"}[5m]))
/ sum(rate(http_request_duration_seconds_count{namespace="tasuki-prod",service="orders",method="POST",route="/v1/orders"}[5m]))

# p95 and p99, per pod
histogram_quantile(0.95, sum by (le, pod) (rate(http_request_duration_seconds_bucket{namespace="tasuki-prod",service="orders",route="/v1/orders"}[5m])))
histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="tasuki-prod",service="orders",route="/v1/orders"}[5m])))

# CPU throttling of the orders pods
sum by (pod) (rate(container_cpu_cfs_throttled_periods_total{namespace="tasuki-prod",pod=~"orders-.*"}[5m]))
```

```bash
kubectl argo rollouts get rollout orders -n tasuki-prod
kubectl -n tasuki-prod logs deploy/orders --tail=500 | jq -c 'select(.route == "/v1/orders" and .duration_ms >= 300)'
```

Traces (Tempo) of slow `POST /v1/orders` requests show where the time goes: the insert transaction (order, outbox,
idempotency record), the connection pool wait, or the quote lookup.

## Mitigate

1. **Recent rollout?** `kubectl argo rollouts abort` (canary) or `undo` (completed) for `orders` in `tasuki-prod`.
2. **Database:** look for lock waits and slow statements (Performance Insights or `pg_stat_statements`). A missing index
   on `idempotency_record` or `outbox` shows up as creates slowing down while traffic is flat.
3. **Connection pool exhausted:** requests queue for a connection. Scale replicas only if the database has headroom;
   otherwise fix the slow query first.
4. **CPU throttled:** raise the CPU limit or scale out.

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

A postmortem is required if one incident burned more than 20 % of the budget
([error-budget policy](../slo/error-budget-policy.md)).
