# Runbook: rollout gate cannot read an error budget

Alert: `RolloutGateBudgetUnknown` (`severity: ticket`): in the last 15 minutes the rollout gate made decisions without
a budget · [rule](../../observability/alerts/rollout-gate.yaml) · [gate](../../tools/rollout-gate/README.md)

## Symptoms

The `cause` label says why:

- `prometheus_error`: the gate could not query Prometheus (connection refused, timeout, HTTP error).
- `no_data`: Prometheus answered, but there is no `slo:period_error_budget_remaining:ratio` series for the service and SLO
  that were asked for.

## Impact

The gate **fails closed** by default: every canary that asked was blocked and its Rollout aborted. Running services are
unaffected; only deploys are stopped.

## Confirm

```bash
# The gate's own view: decision logs carry service, slo, cause and reason
kubectl -n monitoring logs deploy/rollout-gate --tail=200 | jq -c 'select(.msg == "decision" and .cause != "budget")'
# Ask it directly
kubectl -n monitoring exec deploy/rollout-gate -- /rollout-gate check --service <service> --slo <slo>
```

```promql
# Does the series exist? An empty result means the gate is right to say no_data
slo:period_error_budget_remaining:ratio{sloth_service="<service>"}

# Sloth rules loaded? Expect one series per SLO
sloth_slo_info
```

## Mitigate

**`prometheus_error`:**

1. Prometheus down or restarting: fix Prometheus. The gate recovers by itself on the next request.
2. Wrong `PROMETHEUS_URL` in the gate's deployment, or a NetworkPolicy (default deny) blocking gate → Prometheus on 9090.

**`no_data`:**

1. **Typo in the gate's arguments:** `rollout.gate.service` and `rollout.gate.slo` in
   `deploy/apps/overlays/prod/values/<service>.yaml` must match the Sloth spec (`orders` / `availability`, not
   `orders-api` / `avail`). Empty means the release name and every SLO of it.
2. **Rules not loaded:** Prometheus loads every `PrometheusRule` in the cluster (`ruleSelectorNilUsesHelmValues:
   false`), so check that they exist: `kubectl -n monitoring get prometheusrules` should list `rules-generated-<service>`,
   and Argo CD's Application of the same name should be Synced. `sloth_slo_info` lists what Prometheus has loaded.
3. **New service with no traffic yet:** the budget series appears only after real traffic. Run its rollouts with
   `rollout.gate.dryRun: true` in its prod values file (dispatch does, today), and turn it off once
   `rollout-gate check --service <service>` returns a real budget.

**Urgent deploy while the gate is blind:** use the override in the [policy](../slo/error-budget-policy.md#overrides-break-glass)
and record it. Do not switch the gate to `FAIL_MODE=open` to get one deploy out.

## Escalate

Ticket: fix within the day, since deploys are blocked meanwhile. If Prometheus itself is down, that is its own incident.

## Afterwards

If the cause was a misconfigured Rollout, add the service/SLO pair to the gate's CI smoke check, or to the deploy
linting, so the next typo is caught before it reaches the cluster.
