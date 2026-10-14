"""dispatch.events consumer: idempotent by event_id; offsets committed after the effects."""

from __future__ import annotations

import json
import threading
from datetime import timedelta
from typing import Any

import pytest
from django.utils import timezone
from prometheus_client import REGISTRY

from messaging.consumer import process_event, run_consumer
from messaging.models import OutboxEvent, ProcessedEvent
from orders.dispatch_events import HANDLERS
from orders.models import Order, OrderEvent
from tests.helpers import CourierCtx, Tenant, deliver_dispatch_event, dispatch_event, order


def test_courier_assigned_moves_the_order_to_assigned(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    event = dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    assert deliver_dispatch_event(event) == ("CourierAssigned", "applied")
    row = Order.objects.get(pk=order_id)
    assert row.status == "assigned"
    assert row.courier_id == courier.courier.id
    history = list(OrderEvent.objects.filter(order=row).values_list("to_status", "actor"))
    # Dispatch was faster than the relay's commit: the order catches up through dispatching.
    assert history == [
        ("confirmed", f"merchant:{tenant.merchant.id}"),
        ("dispatching", "dispatch"),
        ("assigned", "dispatch"),
    ]


def test_redelivered_event_is_applied_once(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    event = dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    assert deliver_dispatch_event(event)[1] == "applied"
    history_before = OrderEvent.objects.count()
    assert deliver_dispatch_event(event) == ("CourierAssigned", "duplicate")
    assert OrderEvent.objects.count() == history_before
    assert ProcessedEvent.objects.filter(pk=event["event_id"]).count() == 1


def test_same_assignment_under_a_new_event_id_is_harmless(
    tenant: Tenant, courier: CourierCtx
) -> None:
    order_id = order(tenant)["order_id"]
    deliver_dispatch_event(
        dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    )
    again = dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    assert deliver_dispatch_event(again) == ("CourierAssigned", "ignored")


def test_assignment_after_cancellation_is_ignored(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    tenant.client.post(f"/v1/orders/{order_id}/cancel", {}, format="json")
    event = dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    assert deliver_dispatch_event(event)[1] == "ignored"
    assert Order.objects.get(pk=order_id).status == "cancelled"


@pytest.mark.django_db
@pytest.mark.parametrize(
    ("order_id", "courier_known"), [("ord_01JUNKNOWN0000000000000000", True), (None, False)]
)
def test_assignment_for_unknown_order_or_courier_is_ignored(
    tenant: Tenant, courier: CourierCtx, order_id: str | None, courier_known: bool
) -> None:
    target = order_id or order(tenant)["order_id"]
    courier_id = courier.courier.id if courier_known else "cou_01JUNKNOWN0000000000000000"
    event = dispatch_event("CourierAssigned", target, courier_id=courier_id)
    assert deliver_dispatch_event(event)[1] == "ignored"


@pytest.mark.django_db
def test_events_missing_ids_are_invalid() -> None:
    event = dispatch_event("CourierAssigned", "ord_x")  # courier_id is null
    assert deliver_dispatch_event(event)[1] == "invalid"
    del event["order_id"]
    event["event_id"] = "evt_other"
    assert deliver_dispatch_event({**event, "type": "NoCourierAvailable"})[1] == "invalid"


@pytest.mark.django_db
@pytest.mark.parametrize(
    "raw",
    [
        None,
        b"",
        b"not json",
        b"[]",
        b'{"type": "CourierAssigned"}',
        b'{"event_id": 1, "type": "x"}',
    ],
)
def test_unreadable_messages_are_skipped(raw: bytes | None) -> None:
    assert process_event(raw, HANDLERS) == ("unknown", "invalid")
    assert not ProcessedEvent.objects.exists()


@pytest.mark.django_db
def test_offer_events_are_ignored_without_touching_the_database() -> None:
    event = dispatch_event("OfferSent", "ord_x", courier_id="cou_x", offer_id="off_x")
    assert deliver_dispatch_event(event) == ("OfferSent", "ignored")
    assert not ProcessedEvent.objects.exists()


def test_no_courier_keeps_dispatching_until_the_deadline(tenant: Tenant, settings: Any) -> None:
    order_id = order(tenant)["order_id"]
    event = dispatch_event("NoCourierAvailable", order_id)
    assert deliver_dispatch_event(event) == ("NoCourierAvailable", "waiting")
    assert Order.objects.get(pk=order_id).status == "confirmed"

    too_old = timezone.now() - timedelta(seconds=settings.NO_COURIER_CANCEL_AFTER_SECONDS + 1)
    Order.objects.filter(pk=order_id).update(created_at=too_old, status="dispatching")
    later = dispatch_event("NoCourierAvailable", order_id)
    assert deliver_dispatch_event(later) == ("NoCourierAvailable", "cancelled")
    row = Order.objects.get(pk=order_id)
    assert (row.status, row.status_reason) == ("cancelled", "no_courier_available")
    cancelled = OutboxEvent.objects.filter(key=order_id).order_by("id").last()
    assert cancelled is not None
    assert cancelled.payload["type"] == "OrderCancelled"
    assert cancelled.payload["reason"] == "no_courier_available"


def test_no_courier_after_assignment_is_ignored(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    deliver_dispatch_event(
        dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    )
    assert deliver_dispatch_event(dispatch_event("NoCourierAvailable", order_id))[1] == "ignored"


def test_a_failing_handler_rolls_back_its_claim(
    tenant: Tenant, courier: CourierCtx, monkeypatch: pytest.MonkeyPatch
) -> None:
    order_id = order(tenant)["order_id"]
    event = dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)

    def database_down(*args: Any) -> str:
        raise RuntimeError("database went away")

    monkeypatch.setattr("orders.services.assign_courier", database_down)
    with pytest.raises(RuntimeError):
        deliver_dispatch_event(event)
    assert not ProcessedEvent.objects.exists()  # so the retry is not mistaken for a duplicate
    monkeypatch.undo()
    assert deliver_dispatch_event(event)[1] == "applied"


# ---- the consume loop, with a fake Kafka consumer ------------------------------------------------


class FakeMessage:
    def __init__(self, value: bytes | None, offset: int, error: str | None = None) -> None:
        self._value, self._offset, self._error = value, offset, error

    def error(self) -> str | None:
        return self._error

    def value(self) -> bytes | None:
        return self._value

    def topic(self) -> str:
        return "dispatch.events"

    def partition(self) -> int:
        return 0

    def offset(self) -> int:
        return self._offset


class FakeConsumer:
    def __init__(self, messages: list[FakeMessage], stop: threading.Event) -> None:
        self.queue, self.stop = list(messages), stop
        self.subscribed: list[str] = []
        self.committed: list[int] = []
        self.seeks: list[Any] = []
        self.closed = False

    def subscribe(self, topics: list[str]) -> None:
        self.subscribed = topics

    def poll(self, timeout: float) -> FakeMessage | None:
        if not self.queue:
            self.stop.set()
            return None
        return self.queue.pop(0)

    def commit(self, *, message: FakeMessage, asynchronous: bool) -> None:
        assert asynchronous is False  # commit only once the database has committed
        self.committed.append(message.offset())

    def seek(self, partition: Any) -> None:
        self.seeks.append(partition)

    def close(self) -> None:
        self.closed = True


# The loop calls close_old_connections() like any long-running process; inside the default
# per-test transaction that would drop the connection, so loop tests use real transactions.
@pytest.mark.django_db(transaction=True)
def test_consume_loop_commits_after_each_handled_message(
    tenant: Tenant, courier: CourierCtx
) -> None:
    order_id = order(tenant)["order_id"]
    assigned = dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    stop = threading.Event()
    consumer = FakeConsumer(
        [
            FakeMessage(json.dumps(assigned).encode(), 0),
            FakeMessage(None, 1, error="broker transport failure"),
            FakeMessage(json.dumps(assigned).encode(), 2),  # redelivery
            FakeMessage(b"garbage", 3),
        ],
        stop,
    )
    before = (
        REGISTRY.get_sample_value(
            "orders_dispatch_events_total", {"type": "CourierAssigned", "outcome": "duplicate"}
        )
        or 0.0
    )
    beats: list[int] = []
    run_consumer(
        consumer,
        "dispatch.events",
        HANDLERS,
        stop,
        rewind=lambda m: m.offset(),
        heartbeat=lambda: beats.append(1),
    )
    assert consumer.subscribed == ["dispatch.events"]
    assert consumer.committed == [0, 2, 3]  # the transport error is not a message to commit
    assert consumer.closed
    assert beats
    assert Order.objects.get(pk=order_id).status == "assigned"
    after = REGISTRY.get_sample_value(
        "orders_dispatch_events_total", {"type": "CourierAssigned", "outcome": "duplicate"}
    )
    assert after == before + 1


@pytest.mark.django_db(transaction=True)
def test_consume_loop_rewinds_and_does_not_commit_on_failure(
    tenant: Tenant, courier: CourierCtx, monkeypatch: pytest.MonkeyPatch
) -> None:
    order_id = order(tenant)["order_id"]
    event = dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    monkeypatch.setattr(
        "orders.services.assign_courier", lambda *a: (_ for _ in ()).throw(RuntimeError("down"))
    )
    stop = threading.Event()
    consumer = FakeConsumer([FakeMessage(json.dumps(event).encode(), 7)], stop)
    run_consumer(
        consumer,
        "dispatch.events",
        HANDLERS,
        stop,
        rewind=lambda m: ("rewind", m.offset()),
        retry_backoff_s=0,
    )
    assert consumer.committed == []
    assert consumer.seeks == [("rewind", 7)]
    assert consumer.closed
