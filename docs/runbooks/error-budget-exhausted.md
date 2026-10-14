# Runbook: error budget exhausted

Alert: `SLOErrorBudgetExhausted` (`severity: ticket`): an SLO's 28-day error budget is spent ·
[rule](../../observability/alerts/error-budget.yaml) · [policy](../slo/error-budget-policy.md)

## Symptoms

`slo:period_error_budget_remaining:ratio` for one SLO has been at or below 0 for 15 minutes. The rollout gate now
answers `allowed: false` for that service, so its Argo Rollouts canaries abort at the first step.

## Impact

**Feature freeze for that service.** It has failed its users more than the SLO allows over the last 28 days. Other
services are not affected.

## Confirm

```promql
slo:period_error_budget_remaining:ratio{sloth_service="<service>"}
slo:sli_error:ratio_rate1d{sloth_service="<service>"}       # what spent it, day by day
```

```bash
# What the gate answers (exit 1 = blocked)
kubectl -n monitoring exec deploy/rollout-gate -- /rollout-gate check --service <service>
kubectl argo rollouts get rollout <service> -n tasuki-prod   # a blocked canary shows a failed error-budget analysis
kubectl -n tasuki-prod get analysisruns --sort-by=.metadata.creationTimestamp | grep '^<service>-'   # <rollout>-<hash>-<revision>-<step>
```

## Mitigate

1. **Announce the freeze** to everyone who ships to the service: only reliability fixes, security fixes and risk-reducing
   changes until the budget recovers.
2. **Ship fixes through the override, deliberately.** Open a pull request that sets `rollout.gate.dryRun: true` for
   this service in `deploy/apps/overlays/prod/values/<service>.yaml` (the gate still runs and records its answer, but
   cannot abort the canary; `rollout.gate.enabled: false` removes the step entirely), linking this ticket or the
   postmortem. Revert it after the fix is out. The policy's live-incident override, `kubectl argo rollouts promote
   <service> -n tasuki-prod --full`, only helps while the gate's analysis is still running: once it has failed, the
   Rollout is aborted, and `kubectl argo rollouts retry rollout <service> -n tasuki-prod` runs the steps again, gate
   included.
3. **Rollbacks are not blocked.** Going back to one of the last 3 revisions (`rollbackWindow`) skips the canary steps
   and the gate. Roll back in git (pin the previous `image.tag` in the service's prod values file): Argo CD's
   self-heal reverts `kubectl argo rollouts undo`.
4. **Make sure a postmortem exists** for every incident that burned more than 20 % of the budget.

## Escalate

Whether to lift the freeze early, or count an incident as a measurement bug, is the service owner's decision
([who decides](../slo/error-budget-policy.md#who-decides)).

## Afterwards

The freeze ends when the budget is above 0 again, or when the postmortem's critical action items have shipped and the
owner lifts it, recorded in the postmortem. The window rolls: a big incident stops counting 28 days after it happened.
