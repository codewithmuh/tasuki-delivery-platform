# Runbook: orders availability budget burn

Alert: `OrdersAvailabilityBudgetBurn` (`severity: page` or `ticket`) · SLO: 99.9 % of `POST /v1/quotes` and
`POST /v1/orders` requests not 5xx over 28 days · [spec](../../observability/slo/specs/orders.yaml)

## Symptoms

- **page:** 2 % of the 28-day budget burned in the last hour (≥ 1.34 % errors), or 5 % in 6 hours (≥ 0.56 %).
  Both the long window and its short window (5 min, 30 min) are over the threshold, so it is happening now.
- **ticket:** 10 % of the budget burned in a day or in 3 days. Slow and steady: fix it this week, not tonight.

## Impact

Merchants get 5xx when they ask for a quote or create an order. Retries with the same `Idempotency-Key` are safe, but
merchants who give up lose the delivery. The whole 28-day budget is about 40 minutes of full outage.

## Confirm

PromQL (Grafana Explore, or `kubectl -n monitoring port-forward svc/prometheus-operated 9090`):

```promql
# How fast and how much: burn rate now (1 = exactly on budget) and budget left (1 = untouched, < 0 = overspent)
slo:current_burn_rate:ratio{sloth_id="orders-availability"}
slo:period_error_budget_remaining:ratio{sloth_id="orders-availability"}

# Which route and status code
sum by (route, status) (rate(http_requests_total{namespace="tasuki-prod",service="orders",method="POST",route=~"/v1/quotes|/v1/orders",status=~"5.."}[5m]))

# 503 database_unavailable: PostgreSQL could not serve the request, and why
#   connection / shutdown: failover or restart · read_only: connected to a standby · too_many_connections
#   timeout: a statement ran past DB_STATEMENT_TIMEOUT_MS (slow queries or lock waits, not a failover)
sum by (route, reason) (rate(orders_database_unavailable_total{namespace="tasuki-prod"}[5m]))

# One bad pod or all of them
sum by (pod) (rate(http_requests_total{namespace="tasuki-prod",service="orders",status=~"5.."}[5m]))
```

```bash
kubectl argo rollouts get rollout orders -n tasuki-prod          # a canary in progress or a recent rollout?
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=orders,app.kubernetes.io/component=server
# orders is an Argo Rollout in prod, so select its pods by label (kubectl logs deploy/... only works in dev)
kubectl -n tasuki-prod logs -l app.kubernetes.io/name=orders,app.kubernetes.io/component=server --tail=300 \
  | jq -c 'select(.status >= 500)'                                # JSON access logs, with request_id
kubectl cnpg status orders-db -n tasuki-prod                     # the database: primary, standby, replication
```

## Mitigate

1. **Recent rollout?** Roll back first, investigate second. During a canary: `kubectl argo rollouts abort orders -n
   tasuki-prod` (it only touches the Rollout's status, so Argo CD leaves it alone). After a completed rollout: pin the
   previous `image.tag` in `deploy/apps/overlays/prod/values/orders.yaml` and merge. Argo CD syncs it, and the
   Rollout's `rollbackWindow` skips the canary steps and the gate. Do not use `kubectl argo rollouts undo`: it
   changes the Rollout's spec, and Argo CD's self-heal puts the bad version back within seconds.
2. **Only quotes fail?** Quotes call dispatch `GET /v1/eta`; when it does not answer in time, orders should fall back
   to its local estimate, not fail. See [eta-fallback](eta-fallback.md).
3. **503 `database_unavailable`?** The database is failing over, restarting or overloaded, and clients get
   `Retry-After: 2`. A failover should end within 1–2 minutes (design §10). Check `kubectl cnpg status orders-db -n
   tasuki-prod` and the reasons above: `connection`/`shutdown` for longer than that, or `OrdersDatabaseUnavailable`
   firing, means follow [postgres-failover](postgres-failover.md). `timeout` or `too_many_connections` with a healthy
   cluster is load or a slow query; see [slo-orders-latency](slo-orders-latency.md). `read_only` that does not stop
   means pods are still connected to a demoted primary; restarting the orders pods reconnects them through
   `orders-db-rw`.
4. **Errors stopped, but so did the traffic?** After 120 s of failing database checks (`READINESS_DB_GRACE_SECONDS`)
   every orders pod turns unready. Requests then fail at the connection and never reach Django, so this SLI goes
   quiet and the burn alert can resolve while the outage continues. `OrdersDatabaseUnavailable` keeps paging, and the
   simulator's client-side failures (Tasuki simulator dashboard) show what merchants see.
5. **Overloaded?** Check CPU throttling and the replica count (`kubectl argo rollouts get rollout orders -n
   tasuki-prod`). More replicas means a change to `replicas:` in `deploy/apps/overlays/prod/values/orders.yaml`
   (`kubectl scale` is reverted by Argo CD). Look for one tenant sending far more than the rest (there is no
   per-tenant rate limit yet).

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

If one incident burned more than 20 % of the budget, a blameless postmortem is required
([error-budget policy](../slo/error-budget-policy.md)). Check what is left: below 25 % triggers the policy's next stage.
