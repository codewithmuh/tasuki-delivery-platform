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

# One bad pod or all of them
sum by (pod) (rate(http_requests_total{namespace="tasuki-prod",service="orders",status=~"5.."}[5m]))
```

```bash
kubectl argo rollouts get rollout orders -n tasuki-prod          # a canary in progress or a recent rollout?
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=orders
kubectl -n tasuki-prod logs deploy/orders --tail=500 | jq -c 'select(.status >= 500)'   # JSON logs, with request_id
```

## Mitigate

1. **Recent rollout?** Roll back first, investigate second: `kubectl argo rollouts abort orders -n tasuki-prod` during a
   canary, `kubectl argo rollouts undo orders -n tasuki-prod` after one. Rollbacks skip the canary steps and the gate.
2. **Only quotes fail?** Quotes call dispatch `GET /v1/eta`; when it does not answer in time, orders should fall back
   to its local estimate, not fail. See [eta-fallback](eta-fallback.md).
3. **503s for 1–2 minutes?** A PostgreSQL failover does that by design (design §10). If it lasts longer, check the RDS
   events and the database connection errors in the logs.
4. **Overloaded?** Check CPU throttling and the HPA (`kubectl -n tasuki-prod get hpa`), scale out, and look for one tenant
   sending far more than the rest (rate limits are per tenant).

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

If one incident burned more than 20 % of the budget, a blameless postmortem is required
([error-budget policy](../slo/error-budget-policy.md)). Check what is left: below 25 % triggers the policy's next stage.
