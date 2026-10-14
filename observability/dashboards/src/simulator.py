"""Simulator: the city as Tasuki's clients see it, next to the platform's own numbers.

On a game day the first question is "is the simulator broken, or the platform?". Each comparison panel puts the
client-side metric (simulator_*) next to the server-side one, so a gap between them points at the side at fault.
"""

from __future__ import annotations

from grafana import (
    Dashboard,
    dashboard,
    datasource_var,
    lines,
    namespace_var,
    q,
    stat,
    steps,
    timeseries,
)

UID = "tasuki-simulator"
PER_MIN = "suffix:/min"
# Calls worth a latency percentile: seeding (admin_*), readiness polling and the long-lived offer stream are not.
CALLS = 'namespace="$namespace", call!~"admin_.*|orders_readyz|offer_stream"'


def build() -> Dashboard:
    d = Dashboard(
        uid=UID,
        title="Tasuki simulator",
        description="Load the simulator generates and what it experiences, next to what the platform reports.",
        tags=["simulator", "gameday"],
        variables=[datasource_var(), namespace_var()],
        time_from="now-1h",
    )

    d.row("Is the simulator running the scenario?")
    d.add(
        stat(
            "Simulator targets up",
            """
            Simulator pods Prometheus can scrape. The simulator is scaled to 0 by default; a game day sets
            replicas: 1 and the scenario in the environment's values file. 0 means no load is being generated.
            """,
            [q('sum(up{namespace="$namespace", service="simulator"}) or vector(0)', "up")],
            unit="short",
            thresholds=steps("red", (1, "green")),
            graph=False,
        ),
        w=6,
        h=5,
    )
    d.add(
        stat(
            "Couriers on shift",
            "Simulated couriers online right now, per the scenario's shifts and weather (offline_share).",
            [q('sum(simulator_couriers_active{namespace="$namespace"})', "couriers")],
            unit="short",
        ),
        w=6,
        h=5,
    )
    d.add(
        stat(
            "Orders placed successfully (client view)",
            """
            Orders the orders API confirmed (201) divided by orders the simulated customers tried to place. The
            gap is every failure the client saw: platform errors, timeouts and the simulator's own backpressure.
            """,
            [
                q(
                    'sum(rate(simulator_orders_created_total{namespace="$namespace"}[$__rate_interval]))'
                    ' / sum(rate(simulator_orders_attempted_total{namespace="$namespace"}[$__rate_interval]))',
                    "success",
                )
            ],
            unit="percentunit",
            decimals=1,
            thresholds=steps("red", (0.95, "orange"), (0.99, "green")),
        ),
        w=6,
        h=5,
    )
    d.add(
        stat(
            "Deliveries / min (simulator)",
            "Orders the simulated couriers delivered (deliver call accepted by orders).",
            [
                q(
                    'sum(rate(simulator_deliveries_completed_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "deliveries",
                )
            ],
            unit=PER_MIN,
            decimals=1,
        ),
        w=6,
        h=5,
    )
    d.add(
        timeseries(
            "Demand: scenario target vs attempted vs created (orders / h)",
            """
            The scenario's expected order rate (demand curve × weather, summed over wards) against what the
            customers attempted and what orders confirmed. attempted far below target: the simulator is not
            keeping up (CPU, max_in_flight). created below attempted: orders are failing.
            """,
            [
                q('sum(simulator_demand_orders_per_hour{namespace="$namespace"})', "scenario target"),
                q(
                    'sum(rate(simulator_orders_attempted_total{namespace="$namespace"}[$__rate_interval])) * 3600',
                    "attempted",
                ),
                q(
                    'sum(rate(simulator_orders_created_total{namespace="$namespace"}[$__rate_interval])) * 3600',
                    "created",
                ),
            ],
            unit="suffix:/h",
        ),
        w=24,
        h=8,
    )

    d.row("Orders: client view vs platform view")
    d.add(
        timeseries(
            "Failed orders / min by stage and reason",
            """
            Why orders failed, as the simulator saw it. stage: quote or order. reason: http_<status>, timeout,
            network, bad_response, or backpressure: the simulator's own limit on in-flight orders (max_in_flight),
            which is a simulator problem, not a platform one.
            """,
            [
                q(
                    'sum by (stage, reason) (rate(simulator_orders_failed_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{stage}}: {{reason}}",
                )
            ],
            unit=PER_MIN,
            stack=True,
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "Create-order p95: client vs server, vs 300 ms",
            """
            p95 of POST /v1/orders measured by the simulator (client, including network, load balancer and
            retries' first attempts) and by the orders service (server). Both high: the platform is slow. Only
            the client high: the network path or the simulator itself is the problem. Dashed: the 300 ms SLO.
            """,
            [
                q(
                    'histogram_quantile(0.95, sum by (le) (rate(simulator_client_request_duration_seconds_bucket{namespace="$namespace", call="order"}[$__rate_interval])))',
                    "client (simulator)",
                ),
                q(
                    'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="$namespace", service="orders", method="POST", route="/v1/orders"}[$__rate_interval])))',
                    "server (orders)",
                ),
            ],
            unit="s",
            thresholds=lines((0.3, "red")),
            links=[dashboard("Service RED: orders", "tasuki-service-red", "&var-service=orders")],
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "Client latency p95 by call",
            """
            Every API call's p95 as the simulator measured it: quote, order, pickup, deliver (orders), locations
            (fleet-ingest) and route (OSRM). Seeding calls and the long-lived offer stream are left out.
            """,
            [
                q(
                    f"histogram_quantile(0.95, sum by (le, call) (rate(simulator_client_request_duration_seconds_bucket{{{CALLS}}}[$__rate_interval])))",
                    "{{call}}",
                )
            ],
            unit="s",
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "Unsuccessful calls / min by call and outcome",
            """
            Calls that did not end in a 2xx, by outcome: an HTTP status, timeout or network. A 503 from
            locations is fleet-ingest shedding load on purpose; 409 on accept is an offer lost to expiry.
            """,
            [
                q(
                    'sum by (call, outcome) (rate(simulator_client_request_duration_seconds_count{namespace="$namespace", outcome!~"2.."}[$__rate_interval])) * 60',
                    "{{call}}: {{outcome}}",
                )
            ],
            unit=PER_MIN,
            stack=True,
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "Retries and idempotent replays / min",
            """
            Calls retried after a timeout, a 429 or a 5xx, by call. Orders are retried with the same
            Idempotency-Key, so a replay (the API answered from its idempotency store) is a retry that proved
            the first attempt had in fact succeeded.
            """,
            [
                q(
                    'sum by (call) (rate(simulator_request_retries_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "retries: {{call}}",
                ),
                q(
                    'sum(rate(simulator_orders_replayed_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "replays seen by the simulator",
                ),
                q(
                    'sum(rate(orders_idempotent_replays_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "replays served by orders",
                ),
            ],
            unit=PER_MIN,
        ),
        w=12,
        h=8,
    )

    d.row("Couriers, offers and deliveries")
    d.add(
        timeseries(
            "Simulated couriers by state",
            """
            Online simulated couriers by what they are doing: idle, to_store, at_store (waiting for food),
            to_dropoff. Mostly idle while orders wait points at dispatch; mostly at_store points at slow kitchens.
            """,
            [q('sum by (state) (simulator_couriers{namespace="$namespace"})', "{{state}}")],
            unit="short",
            stack=True,
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Offers / min: courier side",
            """
            Offers as the simulated couriers handled them: received, accepted, declined (too far), lost (409:
            expired or taken while deciding), failed (the accept call errored), cancelled (withdrawn).
            """,
            [
                q(
                    'sum by (outcome) (rate(simulator_offers_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{outcome}}",
                )
            ],
            unit=PER_MIN,
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Offers / min: dispatch side",
            """
            The same offers as dispatch counted them: sent, accepted, declined, expired (no answer in 30 s),
            withdrawn (the order was cancelled). Dispatch sent more than couriers received: offers are lost on
            the SSE stream between them.
            """,
            [
                q(
                    'sum by (outcome) (rate(dispatch_offers_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{outcome}}",
                )
            ],
            unit=PER_MIN,
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Deliveries / min: simulator vs platform",
            """
            Deliveries the simulator completed against orders that reached `delivered` in the orders service.
            The lines should match; a gap means deliver calls are failing (see unsuccessful calls).
            """,
            [
                q(
                    'sum(rate(simulator_deliveries_completed_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "simulator",
                ),
                q(
                    'sum(rate(orders_status_transitions_total{namespace="$namespace", status="delivered"}[$__rate_interval])) * 60',
                    "orders: delivered",
                ),
            ],
            unit=PER_MIN,
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Jobs abandoned / min by reason",
            """
            Accepted jobs a simulated courier gave up: offer_cancelled, order_gone, or a pickup / deliver call
            that was rejected or failed. Rejections point at the orders state machine; failures at availability.
            """,
            [
                q(
                    'sum by (reason) (rate(simulator_jobs_abandoned_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{reason}}",
                )
            ],
            unit=PER_MIN,
            stack=True,
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Routes planned / min by source",
            """
            Courier routes the simulator planned through OSRM, or with straight lines when OSRM was unavailable.
            A rise in straight_line here and in dispatch's ETA fallback at the same time means OSRM itself is down.
            """,
            [
                q(
                    'sum by (source) (rate(simulator_routes_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{source}}",
                )
            ],
            unit=PER_MIN,
            stack=True,
        ),
        w=8,
        h=8,
    )

    d.row("GPS")
    d.add(
        timeseries(
            "GPS batches / min by result",
            """
            Location batches the simulated phones posted: sent (accepted), shed (503, dropped as a real app
            would), failed (network or timeout, points kept for the next batch), rejected (4xx).
            """,
            [
                q(
                    'sum by (result) (rate(simulator_location_batches_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{result}}",
                )
            ],
            unit=PER_MIN,
            stack=True,
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "GPS points / s: simulator vs fleet-ingest",
            """
            Points the simulator counted as accepted, dropped or lost, against what fleet-ingest says it
            accepted. simulator accepted ≈ fleet-ingest accepted when only one simulator runs; lost points are
            the price of shedding.
            """,
            [
                q(
                    'sum by (result) (rate(simulator_location_points_total{namespace="$namespace"}[$__rate_interval]))',
                    "simulator: {{result}}",
                ),
                q(
                    'sum(rate(fleet_ingest_points_total{namespace="$namespace", result="accepted"}[$__rate_interval]))',
                    "fleet-ingest: accepted",
                ),
            ],
            unit="suffix:points/s",
            links=[dashboard("Location pipeline", "tasuki-location")],
        ),
        w=12,
        h=8,
    )
    return d
