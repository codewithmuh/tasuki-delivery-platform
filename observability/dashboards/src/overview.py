"""Tasuki overview: the demo screen. Is the city being served right now, and is any promise at risk?"""

from __future__ import annotations

from grafana import (
    Dashboard,
    alert_annotation,
    bargauge,
    dashboard,
    datasource_var,
    doc,
    lines,
    namespace_var,
    q,
    runbook,
    stat,
    steps,
    table,
    timeseries,
)

UID = "tasuki-overview"
PER_MIN = "suffix:/min"
RUNBOOKS = doc("Runbooks (one per alert)", "docs/runbooks/README.md")


def build() -> Dashboard:
    d = Dashboard(
        uid=UID,
        title="Tasuki overview",
        description="Orders in, couriers out, and every SLO at a glance (docs/design.md §4, §9).",
        tags=["overview"],
        variables=[datasource_var(), namespace_var()],
        annotations=[alert_annotation("Paging alerts", 'ALERTS{alertstate="firing", severity="page"}', "red")],
    )

    d.row("Right now")
    d.add(
        stat(
            "Orders created / min",
            """
            Orders confirmed by the orders API (201 on POST /v1/orders). The design's normal peak is about
            100 / min (1.7 orders/s); a rainy Friday lunch is about 300 / min.
            """,
            [q('sum(rate(orders_created_total{namespace="$namespace"}[$__rate_interval])) * 60', "orders / min")],
            unit=PER_MIN,
            decimals=1,
        ),
        w=4,
        h=5,
    )
    d.add(
        stat(
            "Deliveries / min",
            """
            Orders that reached `delivered` (the courier's proof of delivery was accepted). If this falls while
            orders keep coming in, couriers are the bottleneck: the rain-surge case in design §4.
            """,
            [
                q(
                    'sum(rate(orders_status_transitions_total{namespace="$namespace", status="delivered"}[$__rate_interval])) * 60',
                    "deliveries / min",
                )
            ],
            unit=PER_MIN,
            decimals=1,
        ),
        w=4,
        h=5,
    )
    d.add(
        stat(
            "Couriers online",
            """
            Couriers in the Redis set `geo:couriers:all`, as dispatch samples it every 5 s (every dispatch replica
            reports the same number, hence max, not sum). While dispatch is not running, the panel falls back to
            the couriers the tracking service knows.
            """,
            [
                q(
                    'max(dispatch_couriers{namespace="$namespace", state="online"})'
                    ' or max(tracking_couriers{namespace="$namespace"})',
                    "online",
                )
            ],
            unit="short",
        ),
        w=4,
        h=5,
    )
    d.add(
        stat(
            "Couriers available",
            """
            Online couriers with no job (`geo:couriers:available`): the supply dispatch can match right now.
            Zero while orders wait means every new order waits for a courier to finish a delivery.
            """,
            [q('max(dispatch_couriers{namespace="$namespace", state="available"})', "available")],
            unit="short",
            thresholds=steps("red", (1, "orange"), (10, "green")),
        ),
        w=4,
        h=5,
    )
    d.add(
        stat(
            "Orders waiting for a courier",
            """
            Orders dispatch owns that have no accepted courier yet, summed over the 23 wards. A growing number
            means demand is beating supply, and the dispatch-speed SLO (60 s) is next to suffer.
            """,
            [q('sum(dispatch_pending_orders{namespace="$namespace"})', "waiting")],
            unit="short",
            thresholds=steps("green", (50, "orange"), (200, "red")),
            links=[runbook("slo-dispatch-assignment-speed")],
        ),
        w=4,
        h=5,
    )
    d.add(
        stat(
            "Paging alerts firing",
            """
            Tasuki alerts with severity `page` firing now: SLO fast burns, outbox lag, Kafka consumer lag,
            stalled tracking. Anything above zero is an incident; the table at the bottom names them.
            """,
            [
                q(
                    'count(ALERTS{alertstate="firing", severity="page", category!=""}) or vector(0)',
                    "paging",
                    instant=True,
                )
            ],
            unit="short",
            thresholds=steps("green", (1, "red")),
            graph=False,
            links=[RUNBOOKS],
        ),
        w=4,
        h=5,
    )

    d.row("Demand and supply")
    d.add(
        timeseries(
            "Orders, deliveries and cancellations / min",
            """
            The order funnel over time. A quote comes before each order and is valid for 2 minutes. Cancellations
            include orders that no courier took within 15 minutes (reason `no_courier_available`).
            """,
            [
                q('sum(rate(orders_quotes_created_total{namespace="$namespace"}[$__rate_interval])) * 60', "quotes"),
                q('sum(rate(orders_created_total{namespace="$namespace"}[$__rate_interval])) * 60', "orders created"),
                q(
                    'sum(rate(orders_status_transitions_total{namespace="$namespace", status="delivered"}[$__rate_interval])) * 60',
                    "delivered",
                ),
                q(
                    'sum(rate(orders_status_transitions_total{namespace="$namespace", status="cancelled"}[$__rate_interval])) * 60',
                    "cancelled",
                ),
            ],
            unit=PER_MIN,
            links=[dashboard("Orders, outbox & dispatch", "tasuki-orders")],
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "Couriers online by status",
            """
            Available (no job) and busy (on a job) couriers, stacked, so the top of the stack is everyone online.
            From dispatch's 5-second samples of the Redis geo sets that fleet-state maintains.
            """,
            [
                q('max(dispatch_couriers{namespace="$namespace", state="available"})', "available"),
                q(
                    'max(dispatch_couriers{namespace="$namespace", state="online"})'
                    ' - max(dispatch_couriers{namespace="$namespace", state="available"})',
                    "busy",
                ),
            ],
            unit="short",
            stack=True,
            links=[dashboard("Simulator: couriers by state", "tasuki-simulator")],
        ),
        w=12,
        h=8,
    )

    d.row("Promises: dispatch, ETA, freshness")
    d.add(
        timeseries(
            "Time to an accepted courier (p50, p95) vs 60 s",
            """
            From order confirmation to a courier accepting the offer, for orders that got one. The dispatch-speed
            SLO wants 95 % of orders under 60 s (dashed line), so a p95 above the line means the SLO is burning.
            Orders that end with no courier count against the SLO but are not in these percentiles.
            """,
            [
                q(
                    """
                    histogram_quantile(0.5, sum by (le) (
                      rate(dispatch_assignment_duration_seconds_bucket{namespace="$namespace", outcome="assigned"}[$__rate_interval])
                    ))
                    """,
                    "p50",
                ),
                q(
                    """
                    histogram_quantile(0.95, sum by (le) (
                      rate(dispatch_assignment_duration_seconds_bucket{namespace="$namespace", outcome="assigned"}[$__rate_interval])
                    ))
                    """,
                    "p95",
                ),
            ],
            unit="s",
            thresholds=lines((60, "red")),
            links=[runbook("slo-dispatch-assignment-speed")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "ETA fallback ratio",
            """
            Share of ETAs made without road times. orders: quotes that used the local estimate because dispatch's
            GET /v1/eta did not answer within 300 ms. dispatch: ETAs from straight-line distance × a Tokyo detour
            factor because OSRM failed. ETAFallbackHigh opens a ticket above 10 % (dashed line).
            """,
            [
                q(
                    """
                    sum(rate(orders_quotes_created_total{namespace="$namespace", eta_source="fallback"}[$__rate_interval]))
                    / sum(rate(orders_quotes_created_total{namespace="$namespace"}[$__rate_interval]))
                    """,
                    "quotes without a dispatch ETA (orders)",
                ),
                q(
                    """
                    sum(rate(dispatch_eta_requests_total{namespace="$namespace", source="fallback"}[$__rate_interval]))
                    / sum(rate(dispatch_eta_requests_total{namespace="$namespace"}[$__rate_interval]))
                    """,
                    "ETAs without OSRM (dispatch)",
                ),
            ],
            unit="percentunit",
            max_=1,
            thresholds=lines((0.1, "orange")),
            links=[runbook("eta-fallback")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Dispatch outcomes / min",
            """
            How dispatching ends for each order: a courier accepted (`assigned`) or no courier was available
            (`no_courier`). More `no_courier` with flat demand means couriers went offline (rain) or are all busy.
            """,
            [
                q(
                    'sum by (outcome) (rate(dispatch_assignment_duration_seconds_count{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{outcome}}",
                ),
            ],
            unit=PER_MIN,
            stack=True,
            links=[runbook("slo-dispatch-assignment-speed")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Location freshness: GPS point → Redis (p50, p99) vs 2 s",
            """
            Time from fleet-ingest receiving a GPS point to fleet-state writing it to Redis, where dispatch reads
            it. The SLO wants 99 % of points within 2 s, so a p99 above the dashed line burns its budget.
            """,
            [
                q(
                    'histogram_quantile(0.5, sum by (le) (rate(fleet_state_freshness_seconds_bucket{namespace="$namespace"}[$__rate_interval])))',
                    "p50",
                ),
                q(
                    'histogram_quantile(0.99, sum by (le) (rate(fleet_state_freshness_seconds_bucket{namespace="$namespace"}[$__rate_interval])))',
                    "p99",
                ),
            ],
            unit="s",
            thresholds=lines((2, "red")),
            links=[runbook("slo-fleet-state-location-freshness"), dashboard("Location pipeline", "tasuki-location")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Tracking freshness: event age at the viewer (p50, p99) vs 10 s",
            """
            Age of each courier position (now − received_at) when tracking writes it to a customer's or the ops
            console's stream. The SLO wants 99 % of events younger than 10 s (dashed line).
            """,
            [
                q(
                    'histogram_quantile(0.5, sum by (le) (rate(tracking_event_age_seconds_bucket{namespace="$namespace"}[$__rate_interval])))',
                    "p50",
                ),
                q(
                    'histogram_quantile(0.99, sum by (le) (rate(tracking_event_age_seconds_bucket{namespace="$namespace"}[$__rate_interval])))',
                    "p99",
                ),
            ],
            unit="s",
            thresholds=lines((10, "red")),
            links=[runbook("slo-tracking-freshness"), dashboard("Location pipeline", "tasuki-location")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "GPS points / s: accepted and shed",
            """
            Points fleet-ingest accepted and published to Kafka, and points it refused with 503 because Kafka
            could not take them (load shedding, design §10). The design peak is about 750 points/s.
            """,
            [
                q(
                    'sum(rate(fleet_ingest_points_total{namespace="$namespace", result="accepted"}[$__rate_interval]))',
                    "accepted",
                ),
                q(
                    'sum(rate(fleet_ingest_points_total{namespace="$namespace", result="shed"}[$__rate_interval]))',
                    "shed (503)",
                ),
            ],
            unit="suffix:points/s",
            links=[dashboard("Location pipeline", "tasuki-location")],
        ),
        w=8,
        h=8,
    )

    d.row("Error budgets and alerts")
    d.add(
        bargauge(
            "Error budget remaining (28 days)",
            """
            Share of each SLO's 28-day error budget that is left (Sloth's slo:period_error_budget_remaining:ratio).
            Orange at 25 % or less (SLOErrorBudgetLow: reliability work first). Red at 0 (SLOErrorBudgetExhausted:
            feature freeze, and the rollout gate blocks canaries). SLOs exist for tasuki-prod only, so this panel
            ignores the namespace variable.
            """,
            [q("slo:period_error_budget_remaining:ratio", "{{sloth_service}} · {{sloth_slo}}", instant=True)],
            unit="percentunit",
            min_=0,
            max_=1,
            decimals=1,
            thresholds=steps("red", (0, "orange"), (0.25, "green")),
            links=[runbook("error-budget-low"), runbook("error-budget-exhausted"), dashboard("SLOs", "tasuki-slos")],
        ),
        w=10,
        h=9,
    )
    d.add(
        table(
            "Firing alerts",
            """
            Every firing Tasuki alert (the SLO burn-rate alerts and the cause alerts in observability/alerts all
            carry a `category` label), plus any kube-prometheus-stack alert about this namespace. The runbook of
            each alert is listed in docs/runbooks/README.md (link on the alert name).
            """,
            [
                q(
                    'ALERTS{alertstate="firing", category!=""} or ALERTS{alertstate="firing", namespace="$namespace"}',
                    "{{alertname}}",
                    instant=True,
                    fmt="table",
                )
            ],
            hide=["Time", "Value", "__name__", "alertstate", "sloth_id", "sloth_severity"],
            field_links={"alertname": [RUNBOOKS]},
        ),
        w=14,
        h=9,
    )
    return d
