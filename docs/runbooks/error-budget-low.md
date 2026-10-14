# Runbook: error budget low (75 % burned)

Alert: `SLOErrorBudgetLow` (`severity: ticket`): an SLO has 25 % or less of its 28-day error budget left ·
[rule](../../observability/alerts/error-budget.yaml) · [policy](../slo/error-budget-policy.md)

## Symptoms

`slo:period_error_budget_remaining:ratio` for one SLO (labels `sloth_service`, `sloth_slo`) has been at or below 0.25
for 15 minutes. Nothing may be broken right now. This alert is about the last 28 days, not the last 5 minutes.

## Impact

The service has room for about a quarter of its usual failures before the policy freezes its features and the rollout
gate blocks its canaries.

## Confirm

```promql
# Budget left, per SLO
slo:period_error_budget_remaining:ratio

# When was it spent? Plot the daily error ratio over 28 days; spikes are incidents, a raised floor is a chronic problem
slo:sli_error:ratio_rate1d{sloth_service="<service>", sloth_slo="<slo>"}
```

```bash
# The same answer the gate gives a canary
kubectl -n monitoring exec deploy/rollout-gate -- /rollout-gate check --service <service> --slo <slo>
```

## Mitigate

The error-budget policy's 75 % stage:

1. **Reliability work goes first** for this service: move the tickets from its incidents and burn alerts to the top of
   the backlog.
2. **Risky changes need a second reviewer and a written rollback plan** (schema migrations, dependency upgrades,
   hot-path config).
3. **No game days** on this service until the budget is above 50 % again.

## Escalate

Not urgent. Raise it in the weekly SLO review. If the budget keeps falling, the next stage is
[error-budget-exhausted](error-budget-exhausted.md).

## Afterwards

Record in the ticket what spent the budget (incidents or a chronic error rate) and what was done about it.
