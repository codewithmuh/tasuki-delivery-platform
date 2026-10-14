"""Every metric the dashboards query, and where it comes from.

lint.py fails when a dashboard uses a metric that is not listed here, when a listed metric is not used, or when a
metric with in-repo `sources` is missing from those files. `pending=True` marks a metric whose service is still
being written: missing is then a warning, not an error. build.py renders this list into the README.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Metric:
    name: str
    kind: str  # counter | gauge | histogram | recording rule | built-in
    labels: str  # the labels the dashboards use, for the README
    sources: tuple[str, ...] = ()  # repo files or directories whose text must contain `name`
    upstream: str = ""  # the exporter, when the metric does not come from this repository
    pending: bool = False  # the source is not built yet; the name is agreed with its owner


ORDERS = ("services/orders/src/core/metrics.py",)
INGEST = ("services/fleet/internal/ingest/handler.go",)
STATE = ("services/fleet/internal/state/processor.go",)
TRACKING = ("services/fleet/internal/tracking/feed.go",)
DISPATCH = ("services/fleet/internal/dispatch",)
SIMULATOR = ("services/simulator/internal/metrics/metrics.go",)
SLOTH = ("observability/slo/generated",)
HTTP = ("services/orders/src/core/metrics.py", "services/fleet/internal/kit/server.go")

KUBE_STATE_METRICS = "kube-state-metrics (kube-prometheus-stack)"
CADVISOR = "cAdvisor via the kubelet ServiceMonitor (kube-prometheus-stack)"

_SLOTH_WINDOWS = ("5m", "30m", "1h", "2h", "6h", "1d", "3d", "4w")

METRICS: tuple[Metric, ...] = (
    # Runtime contract (docs/architecture.md): every service.
    Metric("http_requests_total", "counter", "service, method, route, status", HTTP),
    Metric("http_request_duration_seconds", "histogram", "service, method, route", HTTP),
    # orders: API, outbox relay, dispatch-event consumer.
    Metric("orders_quotes_created_total", "counter", "eta_source", ORDERS),
    Metric("orders_created_total", "counter", "", ORDERS),
    Metric("orders_idempotent_replays_total", "counter", "", ORDERS),
    Metric("orders_eta_fallbacks_total", "counter", "reason", ORDERS),
    Metric("orders_status_transitions_total", "counter", "status", ORDERS),
    Metric("orders_outbox_lag_seconds", "gauge", "", ORDERS),
    Metric("orders_outbox_pending_events", "gauge", "", ORDERS),
    Metric("orders_outbox_published_total", "counter", "", ORDERS),
    Metric("orders_outbox_publish_failures_total", "counter", "", ORDERS),
    Metric("orders_dispatch_events_total", "counter", "type, outcome", ORDERS),
    # fleet-ingest, fleet-state, tracking.
    Metric("fleet_ingest_points_total", "counter", "result", INGEST),
    Metric("fleet_ingest_shed_requests_total", "counter", "reason", INGEST),
    Metric("fleet_ingest_batch_points", "histogram", "", INGEST),
    Metric("fleet_state_points_total", "counter", "result", STATE),
    Metric("fleet_state_freshness_seconds", "histogram", "", STATE),
    Metric("fleet_state_swept_couriers_total", "counter", "", STATE),
    Metric("fleet_state_rebuilds_total", "counter", "", STATE),
    Metric("fleet_state_last_rebuild_duration_seconds", "gauge", "", STATE),
    Metric("fleet_state_history_errors_total", "counter", "", STATE),
    Metric("tracking_event_age_seconds", "histogram", "stream", TRACKING),
    Metric("tracking_sse_connections", "gauge", "stream", TRACKING),
    Metric("tracking_sse_dropped_events_total", "counter", "stream", TRACKING),
    Metric("tracking_sse_throttled_events_total", "counter", "stream", TRACKING),
    Metric("tracking_records_invalid_total", "counter", "topic", TRACKING),
    Metric("tracking_couriers", "gauge", "pod", TRACKING),
    # dispatch (services/fleet/cmd/dispatch).
    Metric("dispatch_assignment_duration_seconds", "histogram", "outcome", DISPATCH),
    Metric("dispatch_eta_requests_total", "counter", "source", DISPATCH),
    Metric("dispatch_offers_total", "counter", "outcome", DISPATCH),
    Metric("dispatch_pending_orders", "gauge", "ward", DISPATCH),
    Metric("dispatch_couriers", "gauge", "state", DISPATCH),
    Metric("dispatch_outbox_events", "gauge", "", DISPATCH),
    Metric("dispatch_publish_failures_total", "counter", "", DISPATCH),
    Metric("dispatch_order_events_total", "counter", "type, result", DISPATCH),
    Metric("dispatch_paused", "gauge", "pod", DISPATCH),
    Metric("dispatch_offer_streams", "gauge", "", DISPATCH),
    # simulator: the platform as its clients see it.
    Metric("simulator_orders_attempted_total", "counter", "", SIMULATOR),
    Metric("simulator_orders_created_total", "counter", "", SIMULATOR),
    Metric("simulator_orders_failed_total", "counter", "stage, reason", SIMULATOR),
    Metric("simulator_orders_replayed_total", "counter", "", SIMULATOR),
    Metric("simulator_client_request_duration_seconds", "histogram", "call, outcome", SIMULATOR),
    Metric("simulator_request_retries_total", "counter", "call", SIMULATOR),
    Metric("simulator_location_batches_total", "counter", "result", SIMULATOR),
    Metric("simulator_location_points_total", "counter", "result", SIMULATOR),
    Metric("simulator_offers_total", "counter", "outcome", SIMULATOR),
    Metric("simulator_deliveries_completed_total", "counter", "", SIMULATOR),
    Metric("simulator_jobs_abandoned_total", "counter", "reason", SIMULATOR),
    Metric("simulator_couriers_active", "gauge", "", SIMULATOR),
    Metric("simulator_couriers", "gauge", "state", SIMULATOR),
    Metric("simulator_routes_total", "counter", "source", SIMULATOR),
    Metric("simulator_demand_orders_per_hour", "gauge", "", SIMULATOR),
    # Rollout gate (tools/rollout-gate).
    Metric("rollout_gate_decisions_total", "counter", "allowed, cause", ("tools/rollout-gate/internal/server",)),
    # Sloth recording rules (observability/slo/generated).
    *(
        Metric(f"slo:sli_error:ratio_rate{w}", "recording rule", "sloth_id, sloth_service, sloth_slo", SLOTH)
        for w in _SLOTH_WINDOWS
    ),
    Metric("slo:error_budget:ratio", "recording rule", "sloth_id", SLOTH),
    Metric("slo:period_error_budget_remaining:ratio", "recording rule", "sloth_service, sloth_slo", SLOTH),
    # Upstream exporters.
    Metric(
        "kafka_consumergroup_lag",
        "gauge",
        "consumergroup, topic, partition",
        upstream="Kafka Exporter: Strimzi `kafkaExporter` (deploy/apps/base/kafka.yaml), PodMonitor `kafka`",
    ),
    Metric("container_cpu_usage_seconds_total", "counter", "pod, container", upstream=CADVISOR),
    Metric("container_memory_working_set_bytes", "gauge", "pod, container", upstream=CADVISOR),
    Metric("kube_pod_container_resource_requests", "gauge", "pod, resource", upstream=KUBE_STATE_METRICS),
    Metric("kube_pod_container_resource_limits", "gauge", "pod, resource", upstream=KUBE_STATE_METRICS),
    Metric("kube_pod_status_ready", "gauge", "pod, condition", upstream=KUBE_STATE_METRICS),
    Metric("kube_pod_container_status_restarts_total", "counter", "pod", upstream=KUBE_STATE_METRICS),
    Metric("kube_pod_container_status_last_terminated_reason", "gauge", "pod, reason", upstream=KUBE_STATE_METRICS),
    Metric("up", "built-in", "namespace, service", upstream="Prometheus (one series per scrape target)"),
    Metric("ALERTS", "built-in", "alertname, alertstate, severity, category", upstream="Prometheus (firing alerts)"),
)

BY_NAME = {m.name: m for m in METRICS}
HISTOGRAMS = {m.name for m in METRICS if m.kind == "histogram"}
