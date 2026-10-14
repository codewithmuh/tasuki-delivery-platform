"""Outbox relay: publishes, marks published, keeps per-order order, survives Kafka being down."""

from __future__ import annotations

import io
import json
import threading
from collections.abc import Callable
from typing import Any

import pytest
from django.core.management import call_command
from prometheus_client import REGISTRY

from messaging.kafka import KafkaPublisher, rewind_to
from messaging.models import OutboxEvent
from messaging.relay import Message, record_lag, relay_batch, run_relay
from orders.models import Order, OrderEvent
from orders.services import on_events_published
from tests.helpers import FakePublisher, Tenant, order


def _pending() -> int:
    return OutboxEvent.objects.filter(published_at__isnull=True).count()


def test_relay_publishes_marks_and_moves_the_order_on(tenant: Tenant) -> None:
    order_id = order(tenant)["order_id"]
    publisher = FakePublisher()
    result = relay_batch(publisher, on_published=on_events_published)
    assert (result.published, result.failed) == (1, 0)
    assert _pending() == 0

    (message,) = publisher.sent
    assert message.topic == "orders.events"
    assert message.key == order_id  # Kafka key: one order's events stay in one partition
    assert json.loads(message.value)["type"] == "OrderConfirmed"
    headers = dict(message.headers)
    assert headers["event_id"] == message.event_id.encode()
    assert headers["type"] == b"OrderConfirmed"

    row = Order.objects.get(pk=order_id)
    assert row.status == "dispatching"
    assert OrderEvent.objects.filter(order=row, to_status="dispatching", actor="relay").exists()


def test_nothing_to_do(db: None) -> None:
    result = relay_batch(FakePublisher())
    assert (result.published, result.failed) == (0, 0)


def test_unacknowledged_events_stay_pending(tenant: Tenant) -> None:
    order_id = order(tenant)["order_id"]
    before = REGISTRY.get_sample_value("orders_outbox_publish_failures_total") or 0.0
    result = relay_batch(FakePublisher(fail_all=True), on_published=on_events_published)
    assert (result.published, result.failed) == (0, 1)
    assert _pending() == 1
    assert Order.objects.get(pk=order_id).status == "confirmed"
    assert REGISTRY.get_sample_value("orders_outbox_publish_failures_total") == before + 1
    # Kafka is back: the same event goes out.
    assert relay_batch(FakePublisher()).published == 1


def test_partial_acknowledgement(tenant: Tenant) -> None:
    order(tenant)
    order(tenant)
    first = OutboxEvent.objects.order_by("id").first()
    assert first is not None
    result = relay_batch(FakePublisher(drop={first.event_id}))
    assert (result.published, result.failed) == (1, 1)
    first.refresh_from_db()
    assert first.published_at is None


def test_one_event_per_order_per_batch_keeps_order(tenant: Tenant) -> None:
    order_id = order(tenant)["order_id"]
    tenant.client.post(f"/v1/orders/{order_id}/cancel", {}, format="json")
    publisher = FakePublisher()
    assert relay_batch(publisher, on_published=on_events_published).published == 1
    assert relay_batch(publisher, on_published=on_events_published).published == 1
    assert [p["type"] for p in publisher.payloads()] == ["OrderConfirmed", "OrderCancelled"]
    # A cancelled order is not moved to dispatching when its old OrderConfirmed goes out.
    assert Order.objects.get(pk=order_id).status == "cancelled"


def test_head_of_line_is_per_key(tenant: Tenant) -> None:
    a = order(tenant)["order_id"]
    tenant.client.post(f"/v1/orders/{a}/cancel", {}, format="json")
    b = order(tenant)["order_id"]
    publisher = FakePublisher()
    relay_batch(publisher)
    # Order B's event is not stuck behind order A's second event.
    assert [(m.key, json.loads(m.value)["type"]) for m in publisher.sent] == [
        (a, "OrderConfirmed"),
        (b, "OrderConfirmed"),
    ]


def test_batch_size_is_respected(tenant: Tenant) -> None:
    for _ in range(3):
        order(tenant)
    assert relay_batch(FakePublisher(), batch_size=2).published == 2
    assert _pending() == 1


def test_lag_gauges(tenant: Tenant) -> None:
    order(tenant)
    record_lag()
    assert REGISTRY.get_sample_value("orders_outbox_pending_events") == 1
    lag = REGISTRY.get_sample_value("orders_outbox_lag_seconds")
    assert lag is not None
    assert lag >= 0
    relay_batch(FakePublisher())
    record_lag()
    assert REGISTRY.get_sample_value("orders_outbox_pending_events") == 0
    assert REGISTRY.get_sample_value("orders_outbox_lag_seconds") == 0


# The loop calls close_old_connections() like any long-running process; inside the default
# per-test transaction that would drop the connection, so loop tests use real transactions.
@pytest.mark.django_db(transaction=True)
def test_run_relay_until_stopped(tenant: Tenant) -> None:
    order(tenant)
    stop = threading.Event()
    beats: list[int] = []

    def heartbeat() -> None:
        beats.append(1)
        if len(beats) == 2:
            stop.set()

    publisher = FakePublisher()
    run_relay(publisher, stop, poll_interval_s=0, heartbeat=heartbeat)
    assert len(publisher.sent) == 1
    assert _pending() == 0


@pytest.mark.django_db(transaction=True)
def test_run_relay_survives_failures(tenant: Tenant) -> None:
    order(tenant)
    stop = threading.Event()
    calls: list[int] = []

    class Flaky:
        def publish(self, messages: Any) -> set[str]:
            calls.append(1)
            if len(calls) == 1:
                raise RuntimeError("broker down")
            if len(calls) == 2:
                return set()  # nothing acknowledged
            stop.set()
            return {m.event_id for m in messages}

    run_relay(Flaky(), stop, poll_interval_s=0, max_backoff_s=0)
    assert len(calls) == 3
    assert _pending() == 0


# ---- the confluent-kafka adapter, with a fake producer -------------------------------------------


class FakeProducer:
    def __init__(self, fail: set[bytes] | None = None, stuck: int = 0) -> None:
        self.produced: list[dict[str, Any]] = []
        self.callbacks: list[tuple[Callable[[Any, Any], None], bytes]] = []
        self.fail = fail or set()
        self.stuck = stuck
        self.flushes: list[float] = []

    def produce(self, topic: str, **kwargs: Any) -> None:
        self.produced.append({"topic": topic, **kwargs})
        self.callbacks.append((kwargs["on_delivery"], kwargs["key"]))

    def flush(self, timeout: float) -> int:
        self.flushes.append(timeout)
        for callback, key in self.callbacks:
            callback("Local: Message timed out" if key in self.fail else None, None)
        self.callbacks.clear()
        return self.stuck


def _message(event_id: str, key: str) -> Message:
    return Message("orders.events", key, b"{}", event_id, (("event_id", event_id.encode()),))


def test_kafka_publisher_reports_acknowledged_events() -> None:
    producer = FakeProducer(fail={b"ord_2"})
    publisher = KafkaPublisher("unused:9092", delivery_timeout_s=2, producer=producer)
    acked = publisher.publish([_message("evt_1", "ord_1"), _message("evt_2", "ord_2")])
    assert acked == {"evt_1"}
    assert producer.produced[0]["key"] == b"ord_1"
    assert producer.produced[0]["headers"] == [("event_id", b"evt_1")]
    assert producer.flushes == [3]  # waits a little longer than librdkafka's own delivery timeout


def test_kafka_publisher_flush_timeout_and_close(caplog: pytest.LogCaptureFixture) -> None:
    producer = FakeProducer(stuck=1)
    publisher = KafkaPublisher("unused:9092", delivery_timeout_s=1, producer=producer)
    publisher.publish([_message("evt_1", "ord_1")])
    assert "kafka flush timed out" in caplog.text
    publisher.close()
    assert producer.flushes[-1] == 1


def test_kafka_publisher_builds_an_idempotent_producer() -> None:
    publisher = KafkaPublisher("127.0.0.1:1", delivery_timeout_s=0.1)
    assert publisher.publish([]) == set()


def test_rewind_points_at_the_failed_message() -> None:
    class Msg:
        def topic(self) -> str:
            return "dispatch.events"

        def partition(self) -> int:
            return 3

        def offset(self) -> int:
            return 42

    position = rewind_to(Msg())  # type: ignore[arg-type]
    assert (position.topic, position.partition, position.offset) == ("dispatch.events", 3, 42)


# ---- management commands -------------------------------------------------------------------------


def test_relay_outbox_once(tenant: Tenant, monkeypatch: pytest.MonkeyPatch) -> None:
    order(tenant)
    publisher = FakePublisher()
    publisher.close = lambda: None  # type: ignore[attr-defined]
    monkeypatch.setattr(
        "orders.management.commands.relay_outbox.KafkaPublisher", lambda brokers: publisher
    )
    out = io.StringIO()
    call_command("relay_outbox", "--once", stdout=out)
    assert out.getvalue().strip() == "published 1, failed 0"
    assert _pending() == 0


def test_relay_outbox_loop_wiring(db: None, monkeypatch: pytest.MonkeyPatch) -> None:
    module = "orders.management.commands.relay_outbox"
    seen: dict[str, Any] = {}

    class Server:
        def shutdown(self) -> None:
            seen["server_shutdown"] = True

    publisher = FakePublisher()
    publisher.close = lambda: seen.setdefault("closed", True)  # type: ignore[attr-defined]
    monkeypatch.setattr(f"{module}.KafkaPublisher", lambda brokers: publisher)
    monkeypatch.setattr(f"{module}.stop_on_signals", lambda stop: None)
    monkeypatch.setattr(f"{module}.serve_worker_endpoints", lambda port, heartbeat: Server())
    monkeypatch.setattr(f"{module}.run_relay", lambda *args, **kwargs: seen.update(kwargs))
    call_command("relay_outbox", "--batch-size", "10")
    assert seen["batch_size"] == 10
    assert seen["on_published"] is on_events_published
    assert seen["closed"]
    assert seen["server_shutdown"]


def test_consume_dispatch_events_wiring(db: None, monkeypatch: pytest.MonkeyPatch) -> None:
    module = "orders.management.commands.consume_dispatch_events"
    seen: dict[str, Any] = {}

    class Server:
        def shutdown(self) -> None:
            seen["server_shutdown"] = True

    monkeypatch.setattr(f"{module}.build_consumer", lambda brokers, group: ("consumer", group))
    monkeypatch.setattr(f"{module}.stop_on_signals", lambda stop: None)
    monkeypatch.setattr(f"{module}.serve_worker_endpoints", lambda port, heartbeat: Server())
    monkeypatch.setattr(
        f"{module}.run_consumer",
        lambda consumer, topic, handlers, stop, **kw: seen.update(
            consumer=consumer, topic=topic, handlers=handlers
        ),
    )
    call_command("consume_dispatch_events")
    assert seen["consumer"] == ("consumer", "orders")
    assert seen["topic"] == "dispatch.events"
    assert set(seen["handlers"]) == {"CourierAssigned", "NoCourierAvailable"}
    assert seen["server_shutdown"]
