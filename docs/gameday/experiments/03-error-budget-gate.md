# GD-03: Bad deploy blocked by the error-budget gate

| | |
|---|---|
| Status | **Planned, not run.** Both phases can run in prod |
| Design | [§9](../../design.md#9-slos) error-budget policy and rollout gate; [error-budget policy](../../slo/error-budget-policy.md); [tools/rollout-gate](../../../tools/rollout-gate/README.md) |
| SLOs at stake | orders availability (spent **on purpose**) |
| Environment | `tasuki-prod` only: budgets exist only there |
| Time box | Phase A 45 minutes, plus 15 minutes for the budget alerts' `for:`; Phase B 30 minutes |
| Scripts | `orders-5xx.sh`, `gate-check.sh`, `watch-slis.sh`, `record-timeline.sh` |

Run this **last** in a prod session: it freezes orders until the cluster is destroyed (see
[the error budget during drills](../README.md#the-error-budget-during-drills)).

## Hypothesis

1. **Burn.** While `POST /v1/orders` fails (the fault below), orders availability drops far below 99.9 %.
   `OrdersAvailabilityBudgetBurn` pages once both the 1 h and 5 m windows exceed 13.44× burn (1.34 % errors).
2. **Policy stages.** `slo:period_error_budget_remaining:ratio{sloth_service="orders",sloth_slo="availability"}` falls
   to or below 0. `SLOErrorBudgetLow` and then `SLOErrorBudgetExhausted` open tickets after their 15-minute `for:`.
3. **The gate says no.** `rollout-gate check --service orders` exits 1 with `cause: budget`, and keeps saying no after
   the fault is removed, because the budget does not recover until bad samples leave the window.
4. **Phase B: a canary is blocked.** A new orders revision in `tasuki-prod` starts a canary whose first step is the
   `error-budget-gate` analysis. The analysis fails (`allowed: false`, `failureLimit: 0`), the Rollout aborts, and the
   stable ReplicaSet keeps serving 100 % of traffic: no new pod gets traffic.
   Five minutes later `RolloutDegraded` opens a ticket.
5. **Phase B: rollbacks and the override still work.** A rollback is a git change (revert the commit, or pin the
   previous `image.tag`); `rollbackWindow: {revisions: 3}` makes a return to a recent revision skip the steps and the
   gate. `kubectl argo rollouts promote orders --full` (break-glass) skips them too, and is recorded.
   (`kubectl argo rollouts undo` edits `spec.template`, which Argo CD's self-heal reverts: not a rollback here.)
6. **Phase B: the gate fails closed.** With `rollout.gate.slo` set to an SLO that does not exist (`avail`), the gate
   answers `cause: no_data`, the canary is blocked although the budget is irrelevant, and `RolloutGateBudgetUnknown`
   opens a ticket.

## The fault

There is no fault-injection switch in the services, so the drill uses a precise database-level fault:
`orders-5xx.sh` runs `REVOKE INSERT ON TABLE orders_order FROM orders` on the `orders-db` primary. Every order
creation then fails inside its transaction (nothing is half-written: the idempotency claim rolls back with it) and
Django answers 500. Quotes, pickups and deliveries keep working. The script re-grants on exit, on Ctrl-C, or after
`--duration`; `--rollback` re-grants if the laptop died mid-drill.

This stands in for "a bad deploy that returns 5xx". Phase B's canary can be any harmless change.

## Steady state

- **Dashboards:** Tasuki SLOs (budget, burn rate), Tasuki orders & outbox, Tasuki service RED (orders).
- **`watch-slis.sh`:**

  ```bash
  scripts/gameday/watch-slis.sh --context tasuki --namespace tasuki-prod --interval 15 \
    --extra 'orders burn rate now=slo:current_burn_rate:ratio{sloth_id="orders-availability"}' \
    --extra 'budget left (availability)=slo:period_error_budget_remaining:ratio{sloth_id="orders-availability"}' \
    --extra 'sim order failures by reason=sum by (reason) (rate(simulator_orders_failed_total{namespace="{ns}",stage="order"}[1m]))'
  ```

- `scripts/gameday/gate-check.sh --context tasuki --namespace tasuki-prod --service orders` exits 0 (allowed).

## Blast radius

`tasuki-prod` orders creation, for `--duration` (default 300 s). Simulated merchants lose those orders after 4
attempts. The orders availability budget is spent for the rest of the session: **the freeze is real** for orders.
Other services are not affected and keep shipping.

## Preconditions

- Preflight GO (`--service orders`, budget above 50 %), and this is the last prod drill of the session.
- Simulator on `normal-weekday` during a meal peak (11:30–13:30 or 18:00–20:30 JST), or `rainy-friday`, so that
  creations are frequent enough to move the 5 m ratio.
- Announcement issue says that orders will be frozen afterwards.
- **Phase B** uses what `deploy/` now has ([deploy/README.md, "Canary releases and the error-budget
  gate"](../../../deploy/README.md#canary-releases-and-the-error-budget-gate)): orders is an Argo Rollout in prod whose
  first step is the ClusterAnalysisTemplate `error-budget-gate`, asking the rollout-gate Service in `monitoring`.
  Check before starting: `kubectl argo rollouts get rollout orders -n tasuki-prod` is Healthy, and
  `kubectl -n monitoring get deploy rollout-gate` is available. The `kubectl argo rollouts` plugin is installed locally.

## Steps: Phase A (budget and gate decision)

```bash
CTX=tasuki NS=tasuki-prod; export ALLOW_PROD=yes
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context $CTX --namespace $NS \
  --experiment gd-03-error-budget-gate --print-path)"
scripts/gameday/preflight.sh --context $CTX --namespace $NS --service orders
scripts/gameday/gate-check.sh --context $CTX --namespace $NS --service orders     # expect exit 0
scripts/gameday/record-timeline.sh mark steady-state; scripts/gameday/record-timeline.sh snapshot
```

1. **Inject:** `scripts/gameday/orders-5xx.sh --context $CTX --namespace $NS --duration 300`. Leave it running.
2. **Checkpoints:**
   - T+1 min: simulator order failures with reason `http_500`; orders 5xx/s above 0; quotes unaffected.
   - T+5 to 10 min: `OrdersAvailabilityBudgetBurn` (`severity=page`) in `ALERTS`. `mark detect` when it fires.
   - T+5 min: the script re-grants by itself. `mark mitigate`. Order creation succeeds again within a minute.
3. `gate-check.sh ... --service orders`: expect **exit 1**, `allowed: false`, `cause: budget`. Note the remaining ratio.
4. Wait 15 minutes. `SLOErrorBudgetExhausted` (and `SLOErrorBudgetLow` before it, or alongside it if the budget fell
   straight through 25 %) should be firing. `snapshot`.
5. Follow [error-budget-exhausted](../../runbooks/error-budget-exhausted.md) as if it were real: announce the freeze in
   the drill issue. Check that the runbook's commands work as written, and note the ones that do not.

## Steps: Phase B (canary)

6. Open a harmless pull request for orders in prod (for example a new `podAnnotations` entry in
   `deploy/apps/overlays/prod/values/orders.yaml`), merge it, and watch:
   `kubectl argo rollouts get rollout orders -n tasuki-prod --watch`. Expect the gate analysis to fail and the Rollout
   to abort, with no canary pod serving. Note the times.
7. `kubectl -n tasuki-prod get analysisrun` and the gate's decision log:
   `kubectl -n monitoring logs deploy/rollout-gate | jq -c 'select(.msg == "decision")'`. Expect `RolloutDegraded`
   about 5 minutes after the abort; follow [broken-secret-stuck-rollout](../../runbooks/broken-secret-stuck-rollout.md)
   for it and [error-budget-exhausted](../../runbooks/error-budget-exhausted.md) for the decision.
8. **Rollback path:** revert the pull request. The previous revision is within `rollbackWindow`, so Argo Rollouts goes
   back without running the steps or the gate. Note how long it took.
9. **Break-glass** (optional, on a second harmless change): `kubectl argo rollouts promote orders -n tasuki-prod --full`,
   with the reason recorded in the drill log as the policy requires. Then revert that change too.
10. **Fail closed:** a pull request setting `rollout.gate.slo: avail` (a name that does not exist) in
    `deploy/apps/overlays/prod/values/orders.yaml`, together with a harmless change. Expect `cause: no_data`, the canary
    blocked, and `RolloutGateBudgetUnknown` within 15 minutes. Revert it.

## Expected observations

| Signal | Expected |
|---|---|
| Orders availability (5 m) during the fault | Far below 99.9 % (creates are about half of quote + create requests, and the simulator retries each failure 4 times) |
| `OrdersAvailabilityBudgetBurn` page | Fires within about 5–10 minutes |
| Budget remaining | ≤ 0. In a young cluster it can go far below 0 (an ephemeral window, see the README) |
| `gate-check.sh` | Exit 0 before, exit 1 after, until the cluster is destroyed |
| Phase B canary | Aborted at step 1, no canary pod serving, stable unaffected; `RolloutDegraded` about 5 minutes later |
| Phase B rollback (revert) | Skips the steps and the gate |

## Abort criteria

In addition to the [global ones](../README.md#abort-criteria-all-experiments):

- Quotes or courier calls fail too: the fault is broader than planned. Run `orders-5xx.sh --rollback`.
- `has_table_privilege('orders','orders_order','INSERT')` is still false 1 minute after the script ended.

## Rollback

`orders-5xx.sh` re-grants by itself. Otherwise: `scripts/gameday/orders-5xx.sh --context $CTX --namespace $NS --rollback`.
The budget cannot be rolled back. The freeze ends when the window rolls (28 days) or the cluster is destroyed; the
service owner may lift it early, recorded as the policy says.

## Alerts and runbooks

| Alert | Should it fire? | Runbook |
|---|---|---|
| `OrdersAvailabilityBudgetBurn` (page) | **Yes** | [slo-orders-availability](../../runbooks/slo-orders-availability.md) |
| `SLOErrorBudgetLow`, `SLOErrorBudgetExhausted` | **Yes**, after 15 minutes | [error-budget-low](../../runbooks/error-budget-low.md), [error-budget-exhausted](../../runbooks/error-budget-exhausted.md) |
| `RolloutDegraded` | **Yes**, about 5 minutes after the gate aborts the canary (step 6) | [broken-secret-stuck-rollout](../../runbooks/broken-secret-stuck-rollout.md) |
| `RolloutGateBudgetUnknown` | Only in step 10 | [rollout-gate-budget-unknown](../../runbooks/rollout-gate-budget-unknown.md) |
| `OrdersLatencyBudgetBurn` | No | — |

`gate-check.sh` runs the same check from a laptop, which is also how Phase A confirms the decision without a deploy.

## Gaps found while planning

- No fault-injection switch in the services: the drill needs database superuser access to create its fault.
- The budget is only as old as the cluster, so one drill exhausts it. That is useful for demonstrating the gate, and
  misleading as a measure (README).
- orders' PreSync migration runs before the gate (deploy/README.md), so a blocked canary still leaves a new schema
  under the stable pods. Harmless for this drill's change; worth remembering for a real one.

## Results

<!-- fill in after the drill -->

| | |
|---|---|
| Date and window (JST) | <!-- fill in after the drill --> |
| Phases run | <!-- A / A+B --> |
| Driver / observer / scribe | <!-- fill in after the drill --> |
| Drill log | <!-- link --> |
| Hypothesis held? | <!-- per numbered point --> |

| Signal | Expected | Observed |
|---|---|---|
| Fault duration (s) | 300 | <!-- fill in after the drill --> |
| Lowest orders availability (5 m) | far below 0.999 | <!-- fill in after the drill --> |
| Time from inject to page | 5–10 min | <!-- fill in after the drill --> |
| Budget remaining before / after | > 0.5 / ≤ 0 | <!-- fill in after the drill --> |
| 28-day-equivalent burn (README formula) | | <!-- fill in after the drill --> |
| `gate-check.sh` exit code before / after | 0 / 1 | <!-- fill in after the drill --> |
| Phase B: canary outcome, traffic on canary | aborted, 0 % | <!-- fill in after the drill --> |
| Alerts that fired, with times | | <!-- fill in after the drill --> |

Surprises: <!-- fill in after the drill -->

Action items: <!-- fill in after the drill -->

Postmortem: <!-- required by policy if > 20 % of the remaining budget burned: link -->
