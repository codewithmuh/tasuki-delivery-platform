"""Orders & outbox: the order path, where correctness beats speed (design §5, §7.1).

Three processes share the orders image: the API (quotes, orders, idempotency), the outbox relay (orders.events)
and the dispatch-event consumer (dispatch.events). Each has a row here.
"""

from __future__ import annotations

from grafana import (
    Dashboard,
    dashboard,
    datasource_var,
    lines,
    namespace_var,
    q,
    runbook,
    stat,
    steps,
    timeseries,
)

UID = "tasuki-orders"
PER_MIN = "suffix:/min"

# The SLI's own selector (observability/slo/specs/orders.yaml), with the namespace variable.
QUOTE_AND_CREATE = 'namespace="$namespace", service="orders", method="POST", route=~"/v1/quotes|/v1/orders"'
CREATE = 'namespace="$namespace", service="orders", method="POST", route="/v1/orders"'


def build() -> Dashboard:
    d = Dashboard(
        uid=UID,
        title="Tasuki orders, outbox & dispatch",
        description="Quotes, orders, idempotency, ETA sources, the transactional outbox, dispatch events and matching.",
        tags=["orders"],
        variables=[datasource_var(), namespace_var()],
    )

    d.row("Quotes and orders (orders API)")
    d.add(
        timeseries(
            "Quotes and orders / min",
            """
            Quotes by where their ETA came from (dispatch, or the local fallback estimate), and orders created.
            Each order needs a quote that is less than 2 minutes old, so quotes ≥ orders.
            """,
            [
                q(
                    'sum by (eta_source) (rate(orders_quotes_created_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "quotes ({{eta_source}} ETA)",
                ),
                q('sum(rate(orders_created_total{namespace="$namespace"}[$__rate_interval])) * 60', "orders created"),
            ],
            unit=PER_MIN,
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "Order status changes / min",
            """
            Transitions of the order state machine (design §8), counted after their transaction commits:
            confirmed → dispatching → assigned → picked_up → delivered, or cancelled / failed.
            """,
            [
                q(
                    'sum by (status) (rate(orders_status_transitions_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{status}}",
                )
            ],
            unit=PER_MIN,
        ),
        w=12,
        h=8,
    )
    d.add(
        stat(
            "Orders per quote",
            """
            Orders created divided by quotes issued in the window. Below 1 is normal (merchants compare quotes,
            quotes expire after 2 minutes); a sudden drop with steady quotes means order creation is failing.
            """,
            [
                q(
                    'sum(rate(orders_created_total{namespace="$namespace"}[$__rate_interval]))'
                    ' / sum(rate(orders_quotes_created_total{namespace="$namespace"}[$__rate_interval]))',
                    "orders per quote",
                )
            ],
            unit="percentunit",
            decimals=1,
            thresholds=steps("orange", (0.5, "green")),
        ),
        w=6,
        h=8,
    )
    d.add(
        timeseries(
            "Idempotent replays / min",
            """
            Order creations answered from a stored idempotency record: a client retried with the same
            Idempotency-Key after a timeout or 5xx, and got the original 201 instead of a second order.
            Replays rise when the API is slow; they prove retries are safe, not that something broke.
            """,
            [
                q(
                    'sum(rate(orders_idempotent_replays_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "replays",
                )
            ],
            unit=PER_MIN,
        ),
        w=6,
        h=8,
    )
    d.add(
        timeseries(
            "ETA fallback ratio (orders and dispatch)",
            """
            orders: quotes that used the local estimate (straight line × 1.35 at 15 km/h + prep time) because
            dispatch's GET /v1/eta did not answer within 300 ms. dispatch: ETAs that used the straight-line
            fallback because OSRM failed. ETAFallbackHigh opens a ticket above 10 % (dashed line).
            """,
            [
                q(
                    """
                    sum(rate(orders_quotes_created_total{namespace="$namespace", eta_source="fallback"}[$__rate_interval]))
                    / sum(rate(orders_quotes_created_total{namespace="$namespace"}[$__rate_interval]))
                    """,
                    "orders: quotes without a dispatch ETA",
                ),
                q(
                    """
                    sum(rate(dispatch_eta_requests_total{namespace="$namespace", source="fallback"}[$__rate_interval]))
                    / sum(rate(dispatch_eta_requests_total{namespace="$namespace"}[$__rate_interval]))
                    """,
                    "dispatch: ETAs without OSRM",
                ),
            ],
            unit="percentunit",
            max_=1,
            thresholds=lines((0.1, "orange")),
            links=[runbook("eta-fallback")],
        ),
        w=6,
        h=8,
    )
    d.add(
        timeseries(
            "ETA fallbacks / min by cause",
            """
            Why ETAs fell back. orders, by reason: timeout (dispatch slower than 300 ms), unavailable
            (connection refused, DNS, 5xx), bad_response. dispatch: ETA requests by source, osrm or fallback.
            """,
            [
                q(
                    'sum by (reason) (rate(orders_eta_fallbacks_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "orders: {{reason}}",
                ),
                q(
                    'sum by (source) (rate(dispatch_eta_requests_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "dispatch: {{source}}",
                ),
            ],
            unit=PER_MIN,
            links=[runbook("eta-fallback")],
        ),
        w=6,
        h=8,
    )

    d.row("Order API SLIs")
    d.add(
        timeseries(
            "Quote and create 5xx ratio vs 0.1 %",
            """
            The orders availability SLI over the dashboard's window: share of POST /v1/quotes and POST /v1/orders
            answered with a 5xx. The SLO allows 0.1 % over 28 days (dashed line). The SLOs dashboard shows the
            burn rates that page.
            """,
            [
                q(
                    f'sum(rate(http_requests_total{{{QUOTE_AND_CREATE}, status=~"5.."}}[$__rate_interval]))'
                    f" / sum(rate(http_requests_total{{{QUOTE_AND_CREATE}}}[$__rate_interval]))",
                    "5xx ratio",
                )
            ],
            unit="percentunit",
            thresholds=lines((0.001, "red")),
            links=[runbook("slo-orders-availability"), dashboard("SLOs", "tasuki-slos")],
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "POST /v1/orders latency (p50, p95, p99) vs 300 ms",
            """
            Server-side duration of order creation. The latency SLO wants 95 % under 300 ms (dashed line), so a
            p95 above it burns the budget. Order creation is one database transaction: order, outbox row and
            idempotency record.
            """,
            [
                q(
                    f"histogram_quantile({phi}, sum by (le) (rate(http_request_duration_seconds_bucket{{{CREATE}}}[$__rate_interval])))",
                    f"p{label}",
                )
                for phi, label in (("0.5", "50"), ("0.95", "95"), ("0.99", "99"))
            ],
            unit="s",
            thresholds=lines((0.3, "red")),
            links=[
                runbook("slo-orders-latency"),
                dashboard("Service RED: orders", "tasuki-service-red", "&var-service=orders"),
            ],
        ),
        w=12,
        h=8,
    )

    d.row("Outbox relay → orders.events")
    d.add(
        timeseries(
            "Outbox lag (oldest unpublished event)",
            """
            Age of the oldest outbox row the relay has not published to Kafka; 0 when the outbox is empty.
            OrdersOutboxLagHigh pages above 60 s for 5 minutes (dashed line): confirmed orders are not reaching
            dispatch, and the dispatch SLO cannot see them until they do.
            """,
            [q('max(orders_outbox_lag_seconds{namespace="$namespace"})', "lag")],
            unit="s",
            thresholds=lines((60, "red")),
            links=[runbook("orders-outbox-lag")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Unpublished outbox events",
            """
            Outbox rows waiting for the relay. During a Kafka outage they pile up in PostgreSQL and drain later
            (design §10): orders are still accepted, only their events are late.
            """,
            [q('max(orders_outbox_pending_events{namespace="$namespace"})', "pending")],
            unit="short",
            links=[runbook("orders-outbox-lag")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Relay publishes / s and failures",
            """
            Events the relay published to orders.events (acknowledged by Kafka), and events Kafka did not
            acknowledge. Failed events stay pending and are retried, so failures delay events but never lose
            them.
            """,
            [
                q('sum(rate(orders_outbox_published_total{namespace="$namespace"}[$__rate_interval]))', "published"),
                q(
                    'sum(rate(orders_outbox_publish_failures_total{namespace="$namespace"}[$__rate_interval]))',
                    "failed (retried)",
                ),
            ],
            unit="suffix:/s",
            links=[runbook("orders-outbox-lag")],
        ),
        w=8,
        h=8,
    )

    d.row("Consumers: dispatch.events into orders, orders.events into dispatch")
    d.add(
        timeseries(
            "dispatch.events handled / min by type and outcome",
            """
            Messages the orders dispatch-event consumer handled. applied: the order changed (CourierAssigned).
            waiting / cancelled: NoCourierAvailable left the order dispatching, or cancelled it after 15 minutes.
            duplicate: already handled (at-least-once delivery, deduplicated on event_id). ignored: a type orders
            does not act on. invalid: unreadable, skipped.
            """,
            [
                q(
                    'sum by (type, outcome) (rate(orders_dispatch_events_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{type}} {{outcome}}",
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
            "Consumer lag on orders.events and dispatch.events (messages)",
            """
            Kafka Exporter lag per consumer group: `orders` on dispatch.events (assignments reach orders late),
            and dispatch's group on orders.events (new orders reach dispatch late). Tracking reads both topics
            without a group, so it does not appear.
            """,
            [
                q(
                    'sum by (consumergroup, topic) (kafka_consumergroup_lag{namespace="$namespace", topic=~"orders.events|dispatch.events"})',
                    "{{consumergroup}} on {{topic}}",
                )
            ],
            unit="short",
        ),
        w=12,
        h=8,
    )

    d.row("dispatch: matching, offers and its own outbox")
    d.add(
        timeseries(
            "Orders waiting for a courier, top 10 wards",
            """
            Orders dispatch owns with no accepted courier yet, for the 10 busiest wards (ward `other` is a store
            outside the 23 wards). Each ward is matched by one replica, so the series are summed over replicas.
            One ward climbing alone is a local courier shortage, not a platform problem.
            """,
            [q('topk(10, sum by (ward) (dispatch_pending_orders{namespace="$namespace"}))', "{{ward}}")],
            unit="short",
            links=[runbook("slo-dispatch-assignment-speed")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Offers / min by outcome",
            """
            Offers dispatch sent to couriers and how they ended: accepted, declined, expired (no answer within
            30 s) or withdrawn (the order was cancelled). Many expiries mean couriers are not receiving offers:
            check the open offer streams.
            """,
            [
                q(
                    'sum by (outcome) (rate(dispatch_offers_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{outcome}}",
                ),
            ],
            unit=PER_MIN,
            links=[runbook("slo-dispatch-assignment-speed")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "orders.events applied by dispatch / min",
            """
            How dispatch consumed orders.events, by event type and result: applied, duplicate (event_id seen
            before: at-least-once delivery, deduplicated) or ignored. The other half of the consumer picture next
            to orders' dispatch.events consumer above.
            """,
            [
                q(
                    'sum by (type, result) (rate(dispatch_order_events_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "{{type}} {{result}}",
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
            "dispatch outbox: events waiting and publish failures",
            """
            dispatch.events waiting in dispatch's Redis outbox for a Kafka acknowledgement (the same global value
            on every replica, hence max), and records Kafka did not acknowledge, which the relay sends again.
            A growing backlog means assignments are not reaching orders and tracking.
            """,
            [
                q('max(dispatch_outbox_events{namespace="$namespace"})', "waiting"),
                q(
                    'sum(rate(dispatch_publish_failures_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "publish failures / min",
                ),
            ],
            unit="short",
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Replicas paused for a Redis rebuild",
            """
            1 while a dispatch replica waits for fleet-state to rebuild Redis and replays its own state after a
            Redis loss (design §10: "new assignments pause for seconds"); its readiness is 503 meanwhile. Longer
            than a minute: see the fleet-state rebuild panels on the location pipeline dashboard.
            """,
            [q('max by (pod) (dispatch_paused{namespace="$namespace"})', "{{pod}}")],
            unit="short",
            max_=1,
            links=[dashboard("Location pipeline", "tasuki-location")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Open courier offer streams",
            """
            Couriers connected to GET /v1/couriers/me/offers/stream (server-sent events), summed over replicas.
            It should follow couriers online; a courier without a stream cannot receive offers.
            """,
            [
                q('sum(dispatch_offer_streams{namespace="$namespace"})', "offer streams"),
                q('max(dispatch_couriers{namespace="$namespace", state="online"})', "couriers online"),
            ],
            unit="short",
        ),
        w=8,
        h=8,
    )
    return d
