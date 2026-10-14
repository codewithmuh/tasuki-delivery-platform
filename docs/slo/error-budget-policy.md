# Error-budget policy

- Status: **Draft: owner to review**
- Owner: Muhammad Rashid ([@codewithmuh](https://github.com/codewithmuh))
- Last updated: 2026-10-14
- Applies to: every production SLO in [observability/slo/specs](../../observability/slo/specs/)

## Why we have one

An SLO says how unreliable a service may be: 99.9 % available means 0.1 % of requests may fail. That 0.1 % is the
**error budget**. We spend it on purpose, on deploys, experiments and migrations, because shipping carries risk. This
policy says what happens as the budget runs down. The aim is that nobody has to argue, mid-incident, about whether to
keep shipping features.

When budget is left, we ship. When it runs low, reliability work goes first. When it is gone, feature work on that
service stops until the service is reliable again.

## What each SLO protects

Budgets are counted over a **rolling 28-day window**. 28 days = 40,320 minutes.

| Service · SLO | Protects | Target | Budget over 28 days |
|---|---|---|---|
| orders · availability | Merchants can get a quote and place an order (G1) | 99.9 % | 0.1 % of quote and create requests, the same as about **40 minutes** of full outage |
| orders · latency | Merchant checkouts stay fast | 95 % < 300 ms | 5 % of creates may be slow, about 33.6 hours of everything slow |
| fleet-state · location-freshness | Dispatch matches on where couriers really are (G2) | 99 % ≤ 2 s | 1 % of GPS points may be late, about 6.7 hours |
| tracking · freshness | Customers see a live courier (G3) | 99 % ≤ 10 s | 1 % of position events may be stale, about 6.7 hours |
| dispatch · assignment-speed | Orders get a courier quickly (G2) | 95 % ≤ 60 s | 5 % of on-demand orders, about 2,500 a day at 50,000 orders a day |

The "hours" figures assume every event in that time is bad. Most real incidents are partial, so they spend the budget
more slowly.

**How much is left:** `slo:period_error_budget_remaining:ratio{sloth_service="<service>", sloth_slo="<slo>"}`. 1 means
untouched, 0 means spent, and below 0 means overspent. The rollout gate and the budget alerts read the same series.

## What happens as the budget burns

| Budget burned | Signal | What we do |
|---|---|---|
| **< 50 %** | none | Normal work. Canaries go through the rollout gate, which lets them pass. |
| **50 %** | Weekly SLO review | The service owner names what spent the budget and checks that each cause has a ticket. No process change yet. |
| **75 %** | `SLOErrorBudgetLow` ticket | **Reliability work goes first** for that service: it moves to the top of the backlog. Risky changes (schema migrations, dependency upgrades, hot-path config) need a second reviewer and a written rollback plan. No game days on that service. |
| **100 %** | `SLOErrorBudgetExhausted` ticket | **Feature freeze for that service.** Only reliability fixes, security fixes and changes that reduce risk ship. The **rollout gate blocks the service's canaries** (`tools/rollout-gate`), so the freeze enforces itself. Other services keep shipping. |

**One incident that burns more than 20 % of a budget needs a blameless postmortem**, whatever stage the budget is in.
It is written within 5 working days and its action items get owners and dates. The 20 % is measured from the remaining
ratio before and after the incident window. A fast-burn page (2 % of the budget in an hour) is how such incidents
usually announce themselves.

### The freeze in practice

- A service is frozen if **any** of its SLOs is out of budget. The gate checks every SLO of the service by default.
- Blocked: new features, refactors with no reliability goal, dependency bumps that are not security fixes.
- Allowed, and expected: fixes for what burned the budget, more tests and alerts, capacity changes, security fixes.
  Because the gate cannot tell these apart, they ship through the override below.
- **Rollbacks are never blocked.** Rollouts set `rollbackWindow`, so going back to a recent revision skips the canary
  steps, the gate included.

### Overrides (break-glass)

Overrides are human decisions, never automatic, and every override is written down.

1. **Preferred, for a reliability fix:** a pull request to `deploy/` that removes the gate step from that one service's
   Rollout, linking the incident or postmortem. Revert it once the fix is out. The override is visible in git.
2. **During an incident:** the on-call engineer may run `kubectl argo rollouts promote <rollout> -n tasuki-prod --full`
   to skip the remaining steps and analysis. They post the reason in the incident channel, and the postmortem lists it.

## The gate when the budget is unknown

When the gate cannot read a budget (Prometheus down, rules missing, no traffic yet), it **fails closed**: the canary is
blocked and `RolloutGateBudgetUnknown` opens a ticket. A deploy that waits a few minutes for Prometheus costs little.
A deploy that ships while we are blind costs much more. A brand-new service has no budget series until it gets traffic,
so its first rollout runs the gate in Argo Rollouts `dryRun` mode. Both choices need an ADR (see the open questions below).

## How the budget recovers

There is no monthly reset. The window rolls, so a bad hour stops counting exactly 28 days after it happened. This has
a hard edge: once the window is full, a single large incident keeps the budget spent until it ages out, which can be
up to 28 days. The freeze therefore ends when either of these happens first:

- the remaining budget is above 0 again (above `MIN_BUDGET_REMAINING` if the gate is configured stricter), or
- the postmortem's critical action items have shipped **and** the service owner lifts the freeze. They record the
  decision in the postmortem and remove the gate step through the `deploy/` override above until the budget recovers.

## Exceptions

- **Dependencies count.** An AWS zone failure or a payment-provider outage spent our users' patience too, so it spends
  our budget. The answer is the design response in [design §10](../design.md#10-failure-modes-and-degradation), not
  an exclusion.
- **Measurement bugs do not count.** If an SLI was wrong (bad query, missing bucket, mislabelled metric), we fix the
  SLI in a pull request. If the bug made the budget look spent, the owner may lift the freeze early, as above.
- **Game days and load tests count.** Schedule them only while more than 50 % of the service's budget is left.
- **Security fixes always ship**, through the override if the gate blocks them.

## Who decides

| Decision | Who |
|---|---|
| Ship a fix through the override during an incident | The on-call engineer; tells the service owner |
| Lift a freeze early, or count an incident as a measurement bug | The service owner, recorded in the postmortem |
| Disagreement about either | The head of engineering (in this project, the owner) |
| Change an SLO target or this policy | A pull request reviewed by the service owner. Targets change only after a full 28-day window of data. |

This policy is reviewed every quarter, and after any freeze, to check that it helped.

## Open questions (need ADRs)

- Should the gate fail closed or open when the budget is unknown? This draft says closed.
- Is 0 % remaining the right gate threshold, or should canaries stop earlier, for example at 25 %?
- Should the burn-rate windows stay at Sloth's 28-day defaults (13.44× / 5.6×), or follow the 30-day numbers in
  design §9 (14.4× / 6×)?
