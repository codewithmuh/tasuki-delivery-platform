"""Prometheus metrics: HTTP RED metrics from the runtime contract plus the business counters.

Under gunicorn several worker processes serve /metrics, so prometheus_client runs in multiprocess
mode (PROMETHEUS_MULTIPROC_DIR, set in config/gunicorn.py) and the scrape sums every worker's
samples.
"""

from __future__ import annotations

import os

from prometheus_client import (
    CONTENT_TYPE_LATEST,
    REGISTRY,
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
    multiprocess,
)

# In multiprocess mode each process writes its samples to files in this directory. It must exist
# before the first metric is created, in every process that sees the variable (gunicorn's
# on_starting also empties it at startup).
if _multiproc_dir := os.environ.get("PROMETHEUS_MULTIPROC_DIR"):
    os.makedirs(_multiproc_dir, exist_ok=True)

HTTP_REQUESTS = Counter("http_requests_total", "HTTP requests.", ["method", "route", "status"])
HTTP_LATENCY = Histogram(
    "http_request_duration_seconds",
    "HTTP request latency.",
    ["method", "route"],
    # 0.3 s is a bucket edge because the order-API latency SLO is "95 % of creates under 300 ms".
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1.0, 2.5, 5.0),
)

QUOTES_CREATED = Counter("orders_quotes_created_total", "Quotes issued.", ["eta_source"])
ORDERS_CREATED = Counter("orders_created_total", "Orders created.")
IDEMPOTENT_REPLAYS = Counter(
    "orders_idempotent_replays_total", "Order creations answered from a stored idempotency record."
)
ETA_FALLBACKS = Counter(
    "orders_eta_fallbacks_total",
    "Quotes whose ETA came from the local estimate because dispatch did not answer in time.",
    ["reason"],
)
STATUS_TRANSITIONS = Counter(
    "orders_status_transitions_total", "Order status changes, by new status.", ["status"]
)

# Outbox relay (its own process; it serves these on WORKER_METRICS_PORT).
OUTBOX_LAG = Gauge(
    "orders_outbox_lag_seconds",
    "Age of the oldest unpublished outbox event (0 when the outbox is empty).",
    multiprocess_mode="max",
)
OUTBOX_PENDING = Gauge(
    "orders_outbox_pending_events", "Unpublished outbox events.", multiprocess_mode="max"
)
OUTBOX_PUBLISHED = Counter("orders_outbox_published_total", "Outbox events published to Kafka.")
OUTBOX_PUBLISH_FAILURES = Counter(
    "orders_outbox_publish_failures_total", "Outbox events Kafka did not acknowledge (retried)."
)

# Dispatch-event consumer (its own process).
DISPATCH_EVENTS = Counter(
    "orders_dispatch_events_total", "dispatch.events messages handled.", ["type", "outcome"]
)


def render_latest() -> tuple[bytes, str]:
    """The exposition text for /metrics and its content type."""
    if "PROMETHEUS_MULTIPROC_DIR" in os.environ:
        registry = CollectorRegistry()
        multiprocess.MultiProcessCollector(registry)  # type: ignore[no-untyped-call]
        return generate_latest(registry), CONTENT_TYPE_LATEST
    return generate_latest(REGISTRY), CONTENT_TYPE_LATEST
