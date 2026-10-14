# rollout-gate

A small Go service that answers one question for Argo Rollouts: **may this service start a canary right now?** The
answer comes from the service's SLO error budget. If the budget is spent, the canary is blocked.

## Why error budgets should gate deploys

An SLO of 99.9 % says users accept 0.1 % failure. That 0.1 % is a budget, and change is what spends most of it: most
incidents start with a deploy, a config change or a migration. While budget is left, shipping is the right call; the
budget exists to be spent on progress. Once it is gone, the service has already failed its users more than it
promised. Another feature deploy at that point is a bet made with someone else's money.

Most teams write that rule in a policy and then rely on people to remember it at 6 p.m. on a Friday. The gate makes it
part of the pipeline. The [error-budget policy](../../docs/slo/error-budget-policy.md) says that an exhausted budget
freezes features for that service. Argo Rollouts asks the gate before shifting any traffic, the gate says no, and the
Rollout aborts while the stable version keeps serving. Nobody has to argue about it mid-incident, and the override is
explicit and recorded.

The gate does not replace canary analysis. It looks at the service's budget over 28 days. A canary analysis compares
the new version with the stable one over minutes. You want both: the gate decides whether to try, and the canary
analysis decides whether the attempt is good.

## How it works

```
Argo Rollouts ──GET /v1/gate?service=orders&slo=──▶ rollout-gate ──PromQL──▶ Prometheus
                ◀── {"allowed": false, ...} ────────                         (Sloth recording rules)
```

1. It validates `service` and `slo` (lowercase letters, digits and hyphens, so nothing can be injected into PromQL).
2. It runs an instant query for the Sloth recording rule
   `slo:period_error_budget_remaining:ratio{sloth_service="orders", sloth_slo="availability"}`. That value is 1 when
   the budget is untouched, 0 when it is spent, and negative when it is overspent. With no `slo`, it checks every SLO
   of the service and judges on the one with the least budget left, because the policy freezes a service when any of
   its budgets is spent.
3. It allows the canary when `remaining > MIN_BUDGET_REMAINING` (default 0). If several series come back (an HA
   Prometheus pair, for example), the lowest wins. NaN means no traffic and is skipped.
4. If it cannot read a budget, the **fail mode** decides: closed (the default) blocks, open allows.

It always answers HTTP 200 with a decision, so Argo evaluates `allowed` instead of counting an error. Only invalid
input gets a 400:

```json
{"service":"orders","slo":"latency","allowed":false,"remaining":-0.1,"threshold":0,"cause":"budget",
 "reason":"orders/latency: error budget exhausted (-10.0% remains, the gate needs more than 0.0%); see docs/slo/error-budget-policy.md"}
```

`cause` is `budget`, `no_data` or `prometheus_error`. `remaining` is `null` when the budget is unknown.

### Why fail closed

When Prometheus is down, the gate cannot tell a healthy budget from a spent one. Blocking costs a delayed deploy.
Allowing could ship into an incident nobody can see, while the monitoring that would catch it is itself down. Canaries
are optional work and can wait. Fixes during an outage do not wait on the gate: rollbacks skip it (`rollbackWindow`),
and the policy's override is a deliberate, recorded step. `RolloutGateBudgetUnknown` opens a ticket whenever the gate
decides without a budget, so a blind gate never blocks silently. `FAIL_MODE=open` exists for teams that weigh it the
other way. **This default needs an ADR.**

## Usage

```bash
# As a service (what Argo Rollouts calls)
PROMETHEUS_URL=http://prometheus-operated.monitoring:9090 rollout-gate serve
curl 'http://localhost:8080/v1/gate?service=orders&slo=availability'
curl 'http://localhost:8080/v1/gate?service=orders'          # every SLO of orders

# As a CLI (CI, or a human before a risky change): exit 0 allowed, 1 blocked, 2 bad input or config
rollout-gate check --service orders --slo availability --prometheus-url http://localhost:9090
```

| Variable | Default | Meaning |
|---|---|---|
| `PROMETHEUS_URL` | `http://localhost:9090` | Prometheus base URL (`check` also takes `--prometheus-url`) |
| `MIN_BUDGET_REMAINING` | `0` | Block when the remaining ratio is at or below this, in `[0, 1)`; `0.25` stops canaries once 75 % is burned |
| `FAIL_MODE` | `closed` | `closed` blocks when the budget is unknown, `open` allows |
| `PORT` | `8080` | HTTP port for `serve` |
| `SHUTDOWN_TIMEOUT` | `20s` | Drain time after SIGTERM |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Runtime contract (same as every Tasuki service): `/healthz`, `/readyz` (503 during shutdown, never dependent on
Prometheus), `/metrics` (`http_requests_total`, `http_request_duration_seconds`, `rollout_gate_decisions_total{allowed,cause}`),
JSON logs with `request_id` plus one `decision` line per answer, and graceful shutdown on SIGTERM.

## Argo Rollouts

[examples/analysis-template.yaml](examples/analysis-template.yaml) has an `AnalysisTemplate` that uses the `web`
metric provider (`successCondition: result.allowed == true`, `failureLimit: 0`, one measurement), and a Rollout canary
whose first step runs it. The live copies belong in `deploy/`. To roll it out safely:

- **Introduce it in dry-run mode** (`dryRun: [{metricName: error-budget}]` on the analysis step, or in the
  AnalysisTemplate's `spec`). Argo records the gate's answers without acting on them, so you can see how often it
  would have blocked. Do the same for a brand-new service, which has no budget series until it gets traffic.
- **Set `spec.rollbackWindow`** on the Rollout (for example `revisions: 3`), so rolling back to a recent revision skips
  the canary steps and the gate. A rollback must never wait for a budget.
- **Break-glass:** `kubectl argo rollouts promote <rollout> --full` skips the remaining steps and analysis. The policy
  says who may do that and how it is recorded.

## Development

```bash
make gate-lint    # gofmt, go vet, golangci-lint (same config as the other Go modules)
make gate-test    # go test -race -cover ./...
docker buildx build --platform linux/amd64,linux/arm64 -t rollout-gate:dev tools/rollout-gate
```

The tests run against an `httptest` fake Prometheus: allowed, blocked, threshold edge, several series, NaN, no data,
Prometheus down, HTTP errors, malformed responses, and invalid input.

| File | What it does |
|---|---|
| `cmd/rollout-gate/main.go` | Subcommands, flags and environment, signal handling |
| `internal/gate/gate.go` | Config, PromQL, the decision and the fail mode |
| `internal/server/server.go` | HTTP API, probes, metrics, access logs, graceful shutdown |
