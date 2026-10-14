# Runbook: dispatch assignment speed budget burn

Alert: `DispatchAssignmentSpeedBudgetBurn` (`severity: page` or `ticket`) · SLO: 95 % of on-demand orders with an
accepted courier within 60 s of confirmation, over 28 days · [spec](../../observability/slo/specs/dispatch.yaml)

## Symptoms

- **page:** more than 67 % of orders took over 60 s to get a courier (or got none) over the last hour and 5 minutes,
  or more than 28 % over 6 hours and 30 minutes.
- **ticket:** a slower burn over a day or 3 days.

## Impact

Food waits at the store and ETAs slip. The clock starts at `OrderConfirmed`, so a slow outbox or Kafka counts too.

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

# Are orders reaching dispatch at all? Outbox lag (OrdersOutboxLagHigh pages above 60 s)
max(orders_outbox_lag_seconds{namespace="tasuki-prod"})

# Road times: is dispatch on the straight-line fallback?
sum(rate(dispatch_eta_requests_total{namespace="tasuki-prod",source="fallback"}[5m]))
/ sum(rate(dispatch_eta_requests_total{namespace="tasuki-prod"}[5m]))
```

```bash
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=dispatch
kubectl -n tasuki-prod get leases | grep -i ward            # every ward should have a live holder (P3: fenced leases)
kubectl -n tasuki-prod logs deploy/dispatch --tail=300 | jq -c 'select(.msg | test("NoCourierAvailable|offer expired"; "i"))'
```

## Mitigate

1. **Recent rollout?** `kubectl argo rollouts undo dispatch -n tasuki-prod`.
2. **Outbox lag:** follow [orders-outbox-lag](orders-outbox-lag.md). Dispatch cannot assign orders it has not seen.
3. **A dispatch replica died:** its wards move when the leases expire (about 15 s). If wards stay without a holder,
   restart dispatch.
4. **OSRM down:** dispatch falls back to straight-line ETAs and keeps assigning. See [eta-fallback](eta-fallback.md).
5. **Supply, not software:** confirm that quotes show longer ETAs and the surge fee (design §4). Tell the dispatcher
   on duty. This burn is real and stays in the budget.

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS). For supply,
escalate to the dispatcher on duty.

## Afterwards

A postmortem is required if one incident burned more than 20 % of the budget
([error-budget policy](../slo/error-budget-policy.md)). If the cause was supply, the action items go to the surge
model, not the code.
