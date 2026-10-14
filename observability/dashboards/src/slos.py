"""SLOs: for each SLO, the SLI now and over 28 days, the error budget left, and the burn rate of every alert window.

All numbers come from Sloth's recording rules (observability/slo/generated), so this dashboard shows exactly what
the burn-rate alerts and the rollout gate evaluate. The rules hard-code namespace="tasuki-prod" (only production
has SLOs), so the panels built on them ignore the namespace variable.

Burn rate = error ratio over a window ÷ error budget (1 − objective). 1× spends the budget in exactly 28 days.
Sloth's google-28d alert windows (observability/slo/README.md):
    page    5m and 1h above 13.44×, or 30m and 6h above 5.6×
    ticket  2h and 1d above 2.8×,   or 6h and 3d above 0.93×
Both windows of a pair must be above the threshold for the alert to fire.
"""

from __future__ import annotations

from dataclasses import dataclass

from grafana import (
    Dashboard,
    alert_annotation,
    bargauge,
    datasource_var,
    doc,
    lines,
    namespace_var,
    q,
    runbook,
    stat,
    steps,
    timeseries,
)

UID = "tasuki-slos"
BURN = "suffix:×"

PAGE_FAST, PAGE_SLOW, TICKET_FAST, TICKET_SLOW = 13.44, 5.6, 2.8, 0.93


@dataclass(frozen=True)
class Slo:
    sloth_id: str  # sloth_id label: <service>-<slo>
    objective: float  # percent, as in the spec
    title: str
    sli: str  # what a good event is, for the row's descriptions
    runbook: str  # docs/runbooks/<runbook>.md, same as the alert's runbook_url

    @property
    def budget(self) -> float:
        return 1 - self.objective / 100


# One entry per Sloth SLO. lint.py checks this list against observability/slo/generated: same IDs, objectives
# and runbooks. A new spec there fails the lint until it is added here.
SLOS = (
    Slo(
        "orders-availability",
        99.9,
        "orders · availability (99.9 %)",
        "POST /v1/quotes and POST /v1/orders requests that do not return 5xx",
        "slo-orders-availability",
    ),
    Slo(
        "orders-latency",
        95,
        "orders · latency (95 % < 300 ms)",
        "POST /v1/orders requests served in under 300 ms",
        "slo-orders-latency",
    ),
    Slo(
        "fleet-state-location-freshness",
        99,
        "fleet-state · location freshness (99 % < 2 s)",
        "GPS points written to Redis within 2 s of receipt",
        "slo-fleet-state-location-freshness",
    ),
    Slo(
        "tracking-freshness",
        99,
        "tracking · freshness (99 % < 10 s)",
        "position events younger than 10 s when sent to a viewer",
        "slo-tracking-freshness",
    ),
    Slo(
        "dispatch-assignment-speed",
        95,
        "dispatch · assignment speed (95 % < 60 s)",
        "on-demand orders with an accepted courier within 60 s of confirmation",
        "slo-dispatch-assignment-speed",
    ),
)


def _burn(sloth_id: str, window: str) -> str:
    return (
        f'slo:sli_error:ratio_rate{window}{{sloth_id="{sloth_id}"}}'
        f' / ignoring (sloth_window) slo:error_budget:ratio{{sloth_id="{sloth_id}"}}'
    )


def _slo_row(d: Dashboard, slo: Slo) -> None:
    sel = f'sloth_id="{slo.sloth_id}"'
    links = [runbook(slo.runbook)]
    d.row(slo.title)
    d.add(
        stat(
            "SLI, last hour",
            f"Share of {slo.sli}, over the last hour. Green at or above the {slo.objective} % objective.",
            [q(f"1 - slo:sli_error:ratio_rate1h{{{sel}}}", "SLI 1 h")],
            unit="percentunit",
            decimals=2,
            thresholds=steps("red", (slo.objective / 100, "green")),
            links=links,
        ),
        w=4,
        h=7,
    )
    d.add(
        stat(
            "SLI, 28 days",
            f"""
            Share of {slo.sli}, over the 28-day SLO window. Sloth averages the 5-minute ratios, so a bad quiet
            hour weighs as much as a bad busy hour.
            """,
            [q(f"1 - slo:sli_error:ratio_rate4w{{{sel}}}", "SLI 28 d")],
            unit="percentunit",
            decimals=3,
            thresholds=steps("red", (slo.objective / 100, "green")),
            links=links,
        ),
        w=4,
        h=7,
    )
    d.add(
        stat(
            "Error budget remaining",
            f"""
            Share of the 28-day error budget ({slo.budget:.1%} of events may be bad) still unspent. 25 % or less
            opens SLOErrorBudgetLow; 0 or less opens SLOErrorBudgetExhausted, freezes features and makes the
            rollout gate block this service's canaries.
            """,
            [q(f"slo:period_error_budget_remaining:ratio{{{sel}}}", "budget left")],
            unit="percentunit",
            decimals=1,
            thresholds=steps("red", (0, "orange"), (0.25, "green")),
            links=[*links, runbook("error-budget-low"), runbook("error-budget-exhausted")],
        ),
        w=4,
        h=7,
    )
    d.add(
        timeseries(
            f"Page burn rate (5m & 1h > {PAGE_FAST}×, 30m & 6h > {PAGE_SLOW}×)",
            f"""
            Burn rate per alert window: error ratio ÷ budget. The page alert fires when 5m and 1h are both above
            {PAGE_FAST}× (2 % of the budget in an hour; all of it in about 2 days), or 30m and 6h are both above
            {PAGE_SLOW}× (5 % in 6 hours). The long window proves the burn is real; the short one makes the alert
            stop soon after the fix.
            """,
            [q(_burn(slo.sloth_id, w), w) for w in ("5m", "1h", "30m", "6h")],
            unit=BURN,
            thresholds=lines((PAGE_SLOW, "orange"), (PAGE_FAST, "red")),
            links=links,
        ),
        w=6,
        h=7,
    )
    d.add(
        timeseries(
            f"Ticket burn rate (2h & 1d > {TICKET_FAST}×, 6h & 3d > {TICKET_SLOW}×)",
            f"""
            The slow-burn windows. The ticket alert fires when 2h and 1d are both above {TICKET_FAST}× (10 % of the
            budget in a day), or 6h and 3d are both above {TICKET_SLOW}× (on course to spend the whole budget in
            about 30 days). Fix within working hours.
            """,
            [q(_burn(slo.sloth_id, w), w) for w in ("2h", "1d", "6h", "3d")],
            unit=BURN,
            thresholds=lines((TICKET_SLOW, "yellow"), (TICKET_FAST, "orange")),
            links=links,
        ),
        w=6,
        h=7,
    )


def build() -> Dashboard:
    d = Dashboard(
        uid=UID,
        title="Tasuki SLOs",
        description="Every SLO's SLI, error budget and burn rates, from the Sloth recording rules (design §9).",
        tags=["slo"],
        variables=[datasource_var(), namespace_var()],
        annotations=[
            alert_annotation("Burn-rate alerts", 'ALERTS{alertstate="firing", alertname=~".+BudgetBurn"}', "red")
        ],
        time_from="now-7d",
    )
    policy = doc("Error-budget policy", "docs/slo/error-budget-policy.md")

    d.row("All SLOs")
    d.add(
        bargauge(
            "Error budget remaining (28 days)",
            """
            Share of each SLO's 28-day error budget left. Orange at 25 % or less (reliability work first), red
            at 0 (feature freeze; the rollout gate blocks canaries). See the error-budget policy.
            """,
            [q("slo:period_error_budget_remaining:ratio", "{{sloth_service}} · {{sloth_slo}}", instant=True)],
            unit="percentunit",
            min_=0,
            max_=1,
            decimals=1,
            thresholds=steps("red", (0, "orange"), (0.25, "green")),
            links=[policy, runbook("error-budget-low"), runbook("error-budget-exhausted")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Error budget remaining over time",
            """
            How each budget is being spent. A steady slope is normal background errors; a cliff is an incident.
            Dashed lines: 25 % (SLOErrorBudgetLow) and 0 (SLOErrorBudgetExhausted). The rollout gate reads this
            same series before every canary step.
            """,
            [q("slo:period_error_budget_remaining:ratio", "{{sloth_service}} · {{sloth_slo}}")],
            unit="percentunit",
            min_=None,
            thresholds=lines((0, "red"), (0.25, "orange")),
            links=[policy],
        ),
        w=10,
        h=8,
    )
    d.add(
        bargauge(
            "Burn rate now (last hour)",
            """
            Each SLO's error ratio over the last hour divided by its budget. Below 1× the budget lasts the whole
            28 days. Orange from 1×; red from 5.6×, the slower page threshold.
            """,
            [
                q(
                    "slo:sli_error:ratio_rate1h / ignoring (sloth_window) slo:error_budget:ratio",
                    "{{sloth_service}} · {{sloth_slo}}",
                    instant=True,
                )
            ],
            unit=BURN,
            min_=0,
            decimals=2,
            thresholds=steps("green", (1, "orange"), (PAGE_SLOW, "red")),
        ),
        w=6,
        h=8,
    )

    for slo in SLOS:
        _slo_row(d, slo)

    d.row("Rollout gate (tools/rollout-gate)")
    d.add(
        timeseries(
            "Rollout gate decisions / hour",
            """
            Decisions the rollout gate gave Argo Rollouts canary analyses, by outcome and cause. budget: the
            error budget decided. no_data / prometheus_error: the gate could not read a budget and failed closed
            (canary blocked); RolloutGateBudgetUnknown opens a ticket.
            """,
            [
                q(
                    "sum by (allowed, cause) (rate(rollout_gate_decisions_total[$__rate_interval])) * 3600",
                    "allowed={{allowed}} cause={{cause}}",
                )
            ],
            unit="suffix:/h",
            stack=True,
            links=[runbook("rollout-gate-budget-unknown"), policy],
        ),
        w=24,
        h=7,
    )
    return d
