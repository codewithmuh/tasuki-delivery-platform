"""Location pipeline: courier GPS → fleet-ingest → Kafka courier.locations → fleet-state (Redis) and tracking (SSE).

Design §5: locations are many and disposable, so this path is judged on freshness and completeness, and it is
allowed to shed load. Each row follows one hop of the pipeline.
"""

from __future__ import annotations

from grafana import (
    Dashboard,
    dashboard,
    datasource_var,
    heatmap,
    lines,
    namespace_var,
    q,
    runbook,
    stat,
    steps,
    timeseries,
)

UID = "tasuki-location"
PER_S = "suffix:/s"
PER_MIN = "suffix:/min"


def build() -> Dashboard:
    d = Dashboard(
        uid=UID,
        title="Tasuki location pipeline",
        description="GPS points from couriers to Redis and to viewers: throughput, shedding, lag and freshness.",
        tags=["location", "fleet"],
        variables=[datasource_var(), namespace_var()],
        time_from="now-1h",
    )

    d.row("fleet-ingest: GPS points in")
    d.add(
        timeseries(
            "GPS points / s by result",
            """
            Every point fleet-ingest received, stacked by what happened to it. accepted: published to Kafka.
            invalid / future / duplicate: dropped by validation (a device clock more than 30 s ahead counts as
            future). shed: the whole batch got 503 + Retry-After because Kafka could not take it.
            """,
            [
                q(
                    'sum by (result) (rate(fleet_ingest_points_total{namespace="$namespace"}[$__rate_interval]))',
                    "{{result}}",
                )
            ],
            unit="suffix:points/s",
            stack=True,
            links=[dashboard("Service RED: fleet-ingest", "tasuki-service-red", "&var-service=fleet-ingest")],
        ),
        w=10,
        h=8,
    )
    d.add(
        timeseries(
            "Batches shed with 503 / s by reason",
            """
            Load shedding (design §10): buffer_full (the producer buffer of 10,000 records is full), timeout
            (Kafka did not acknowledge within 2 s) or kafka_error. The courier app drops the batch and sends a
            fresher one, so this path loses points on purpose instead of falling over.
            """,
            [
                q(
                    'sum by (reason) (rate(fleet_ingest_shed_requests_total{namespace="$namespace"}[$__rate_interval]))',
                    "{{reason}}",
                )
            ],
            unit=PER_S,
            stack=True,
        ),
        w=7,
        h=8,
    )
    d.add(
        timeseries(
            "Points per request (p50, p99)",
            """
            GPS points in each accepted request. Normally 1. Larger batches mean courier apps were offline or
            were shed, and are catching up with buffered points.
            """,
            [
                q(
                    'histogram_quantile(0.5, sum by (le) (rate(fleet_ingest_batch_points_bucket{namespace="$namespace"}[$__rate_interval])))',
                    "p50",
                ),
                q(
                    'histogram_quantile(0.99, sum by (le) (rate(fleet_ingest_batch_points_bucket{namespace="$namespace"}[$__rate_interval])))',
                    "p99",
                ),
            ],
            unit="short",
        ),
        w=7,
        h=8,
    )

    d.row("Kafka: courier.locations (needs the Kafka Exporter)")
    d.add(
        timeseries(
            "fleet-state consumer lag (messages)",
            """
            Messages on courier.locations that the fleet-state consumer group has not processed yet, from the
            Kafka Exporter that Strimzi runs. KafkaConsumerLagHigh pages above 7,500 (dashed line, about 10 s of
            peak traffic): a stopped consumer records no freshness events, so lag is the only signal left.
            Tracking reads the topic without a consumer group, so it has no lag here.
            """,
            [
                q(
                    'sum by (consumergroup) (kafka_consumergroup_lag{namespace="$namespace", topic="courier.locations"})',
                    "{{consumergroup}}",
                )
            ],
            unit="short",
            thresholds=lines((7500, "red")),
            links=[runbook("kafka-consumer-lag")],
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "fleet-state lag by partition",
            """
            The same lag per partition (12 partitions, keyed by courier_id). One partition far behind the
            others points at one stuck consumer or a hot key; all of them behind points at Redis or capacity.
            """,
            [
                q(
                    'sum by (partition) (kafka_consumergroup_lag{namespace="$namespace", topic="courier.locations", consumergroup="fleet-state"})',
                    "partition {{partition}}",
                )
            ],
            unit="short",
            links=[runbook("kafka-consumer-lag")],
        ),
        w=12,
        h=8,
    )

    d.row("fleet-state: positions in Redis")
    d.add(
        heatmap(
            "Freshness heatmap: received_at → Redis",
            """
            How long points took from fleet-ingest to Redis, as a heatmap of the fleet_state_freshness_seconds
            buckets. A healthy pipeline is a bright band under 0.5 s; a band moving up means a consumer falling
            behind, a smear means Redis retries.
            """,
            'sum by (le) (increase(fleet_state_freshness_seconds_bucket{namespace="$namespace"}[$__rate_interval]))',
            unit="s",
            links=[runbook("slo-fleet-state-location-freshness")],
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "Share of points in Redis within 2 s (SLI)",
            """
            The location-freshness SLI: points written to Redis within 2 s of receipt, out of all live points
            (Redis rebuilds are not counted). The target is 99 % (dashed line). Below the line, the SLO burns.
            """,
            [
                q(
                    """
                    sum(rate(fleet_state_freshness_seconds_bucket{namespace="$namespace", le="2.0"}[$__rate_interval]))
                    / sum(rate(fleet_state_freshness_seconds_count{namespace="$namespace"}[$__rate_interval]))
                    """,
                    "within 2 s",
                )
            ],
            unit="percentunit",
            min_=None,
            max_=1,
            decimals=2,
            thresholds=lines((0.99, "red")),
            links=[runbook("slo-fleet-state-location-freshness")],
        ),
        w=12,
        h=8,
    )
    d.add(
        stat(
            "Redis rebuilds (time range)",
            """
            Completed rebuilds of Redis from the last 5 minutes of courier.locations. One per pod start is
            expected; more mean Redis lost its data (restart without persistence, failover, FLUSHALL).
            """,
            [q('sum(increase(fleet_state_rebuilds_total{namespace="$namespace"}[$__range]))', "rebuilds")],
            unit="short",
            decimals=0,
            graph=False,
            thresholds=steps("green", (1, "blue")),
        ),
        w=4,
        h=8,
    )
    d.add(
        stat(
            "Last rebuild took",
            """
            Duration of the most recent rebuild. Dispatch pauses new assignments until a rebuild finishes, so
            this is roughly how long matching stopped after Redis was lost (design §10: "seconds").
            """,
            [q('max(fleet_state_last_rebuild_duration_seconds{namespace="$namespace"})', "duration")],
            unit="s",
            graph=False,
            thresholds=steps("green", (10, "orange"), (30, "red")),
        ),
        w=4,
        h=8,
    )
    d.add(
        timeseries(
            "Points written / s by result",
            """
            What fleet-state did with each point. applied: newest for its courier, written. stale: a newer point
            was already stored (out-of-order delivery is harmless by design). invalid: undecodable, skipped.
            failed: the write failed for that point.
            """,
            [
                q(
                    'sum by (result) (rate(fleet_state_points_total{namespace="$namespace"}[$__rate_interval]))',
                    "{{result}}",
                )
            ],
            unit="suffix:points/s",
            stack=True,
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Couriers swept and history errors / min",
            """
            swept: couriers removed from the geo sets because they sent nothing for SEEN_WINDOW (60 s): apps
            closed or phones offline. history errors: batches the history sink could not write. History has the
            lowest priority (design §10), so these never block Redis.
            """,
            [
                q(
                    'sum(rate(fleet_state_swept_couriers_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "swept",
                ),
                q(
                    'sum(rate(fleet_state_history_errors_total{namespace="$namespace"}[$__rate_interval])) * 60',
                    "history errors",
                ),
            ],
            unit=PER_MIN,
        ),
        w=8,
        h=8,
    )

    d.row("tracking: positions to viewers (SSE)")
    d.add(
        timeseries(
            "Open SSE streams by stream",
            """
            Open server-sent event streams: `track` is customers and merchants following one order, `fleet` is
            the ops console watching every courier. Each replica takes up to 5,000 (TRACKING_MAX_VIEWERS), then
            answers 503.
            """,
            [q('sum by (stream) (tracking_sse_connections{namespace="$namespace"})', "{{stream}}")],
            unit="short",
            stack=True,
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Event age at the viewer (p50, p99) vs 10 s",
            """
            Age of each position event (now − received_at) when it is written to a viewer, per stream. The
            tracking-freshness SLO wants 99 % younger than 10 s (dashed line). Snapshots sent on connect are not
            counted.
            """,
            [
                q(
                    'histogram_quantile(0.5, sum by (le, stream) (rate(tracking_event_age_seconds_bucket{namespace="$namespace"}[$__rate_interval])))',
                    "p50 {{stream}}",
                ),
                q(
                    'histogram_quantile(0.99, sum by (le, stream) (rate(tracking_event_age_seconds_bucket{namespace="$namespace"}[$__rate_interval])))',
                    "p99 {{stream}}",
                ),
            ],
            unit="s",
            thresholds=lines((10, "red")),
            links=[runbook("slo-tracking-freshness")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Dropped events for slow viewers (share)",
            """
            Share of position events dropped because a viewer read too slowly and its 64-event queue was full
            (oldest dropped first). Dropped events are never sent, so the freshness SLI cannot see them:
            TrackingSSEDropsHigh opens a ticket above 2 % (dashed line).
            """,
            [
                q(
                    """
                    sum by (stream) (rate(tracking_sse_dropped_events_total{namespace="$namespace"}[$__rate_interval]))
                    / (
                      sum by (stream) (rate(tracking_event_age_seconds_count{namespace="$namespace"}[$__rate_interval]))
                      + sum by (stream) (rate(tracking_sse_dropped_events_total{namespace="$namespace"}[$__rate_interval]))
                    )
                    """,
                    "dropped {{stream}}",
                )
            ],
            unit="percentunit",
            thresholds=lines((0.02, "orange")),
            links=[runbook("tracking-sse-drops")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Events / s: sent, throttled, invalid",
            """
            sent: position events written to viewers. throttled: skipped on purpose by the brownout knob
            TRACKING_MIN_INTERVAL (design §10, rain surge); not drops. invalid: Kafka records tracking could not
            decode, by topic.
            """,
            [
                q(
                    'sum by (stream) (rate(tracking_event_age_seconds_count{namespace="$namespace"}[$__rate_interval]))',
                    "sent {{stream}}",
                ),
                q(
                    'sum by (stream) (rate(tracking_sse_throttled_events_total{namespace="$namespace"}[$__rate_interval]))',
                    "throttled {{stream}}",
                ),
                q(
                    'sum by (topic) (rate(tracking_records_invalid_total{namespace="$namespace"}[$__rate_interval]))',
                    "invalid {{topic}}",
                ),
            ],
            unit=PER_S,
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Fleet-stream events / s by replica",
            """
            Position events each tracking replica sends to ops-console viewers. Every replica reads the whole
            topic, so a replica that holds viewers but sends nothing while couriers report has stalled:
            TrackingFeedStalled pages for exactly that.
            """,
            [
                q(
                    'sum by (pod) (rate(tracking_event_age_seconds_count{namespace="$namespace", stream="fleet"}[$__rate_interval]))',
                    "{{pod}}",
                )
            ],
            unit=PER_S,
            links=[runbook("tracking-feed-stalled")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "Couriers known per tracking replica",
            """
            Couriers with a known position on each replica. All replicas read the same topic, so the lines
            should overlap; one replica below the others is behind on courier.locations.
            """,
            [q('max by (pod) (tracking_couriers{namespace="$namespace"})', "{{pod}}")],
            unit="short",
            links=[runbook("tracking-feed-stalled")],
        ),
        w=8,
        h=8,
    )
    return d
