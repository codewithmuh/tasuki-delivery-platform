"""Concurrency guarantees that only PostgreSQL can show: blocking unique inserts and SKIP LOCKED.

Each test runs real threads with their own database connections against committed data
(`transaction=True`). SQLite serialises all writers and ignores FOR UPDATE, so these are skipped
there.
"""

from __future__ import annotations

import threading
import time
import uuid
from collections.abc import Callable, Sequence
from typing import Any

import pytest
from django.db import connection
from rest_framework.test import APIClient

from messaging.models import OutboxEvent, ProcessedEvent
from messaging.relay import Message, relay_batch
from orders import services
from orders.models import IdempotencyRecord, Order
from tests.helpers import (
    CourierCtx,
    FakePublisher,
    Tenant,
    deliver_dispatch_event,
    dispatch_event,
    order,
    quote,
)

pytestmark = [pytest.mark.postgres, pytest.mark.django_db(transaction=True)]


def _in_threads(*targets: Callable[[], Any]) -> list[Any]:
    """Start the targets together; each gets its own connection, closed when it finishes."""
    results: list[Any] = [None] * len(targets)
    errors: list[BaseException] = []
    barrier = threading.Barrier(len(targets))

    def run(index: int, target: Callable[[], Any]) -> None:
        try:
            barrier.wait()
            results[index] = target()
        except BaseException as exc:
            errors.append(exc)
        finally:
            connection.close()

    threads = [threading.Thread(target=run, args=(i, t)) for i, t in enumerate(targets)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=30)
    assert not errors, errors
    return results


def _slow_create_order(monkeypatch: pytest.MonkeyPatch, seconds: float = 0.5) -> None:
    """Hold the creating transaction open, so the duplicate really arrives while it is in flight."""
    original = services.create_order

    def slow(*args: Any, **kwargs: Any) -> Order:
        created = original(*args, **kwargs)
        time.sleep(seconds)
        return created

    monkeypatch.setattr(services, "create_order", slow)


def _poster(tenant: Tenant, body: dict[str, Any], key: str) -> Callable[[], Any]:
    def post() -> Any:
        client = APIClient()
        client.credentials(HTTP_AUTHORIZATION=f"Bearer {tenant.api_key}")
        return client.post("/v1/orders", body, format="json", HTTP_IDEMPOTENCY_KEY=key)

    return post


def test_concurrent_duplicates_create_one_order(
    tenant: Tenant, monkeypatch: pytest.MonkeyPatch
) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    key = str(uuid.uuid4())
    _slow_create_order(monkeypatch)
    first, second = _in_threads(_poster(tenant, body, key), _poster(tenant, body, key))
    assert first.status_code == second.status_code == 201
    assert first.json() == second.json()
    replayed = [r.headers.get("Idempotent-Replayed") for r in (first, second)]
    assert sorted(replayed, key=str) == [None, "true"]
    assert Order.objects.count() == 1
    assert OutboxEvent.objects.count() == 1
    assert IdempotencyRecord.objects.count() == 1


def test_concurrent_orders_for_one_quote_create_one_order(
    tenant: Tenant, monkeypatch: pytest.MonkeyPatch
) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    _slow_create_order(monkeypatch)
    responses = _in_threads(_poster(tenant, body, "key-a"), _poster(tenant, body, "key-b"))
    assert sorted(r.status_code for r in responses) == [201, 409]
    loser = next(r for r in responses if r.status_code == 409)
    assert loser.json()["error"]["code"] == "quote_already_used"
    assert Order.objects.count() == 1


class BlockingPublisher(FakePublisher):
    """Holds its batch (and so its row locks) until released."""

    def __init__(self) -> None:
        super().__init__()
        self.holding = threading.Event()
        self.release = threading.Event()

    def publish(self, messages: Sequence[Message]) -> set[str]:
        self.holding.set()
        assert self.release.wait(10)
        return super().publish(messages)


def _relay_in_background(publisher: BlockingPublisher, batch_size: int) -> threading.Thread:
    def run() -> None:
        try:
            relay_batch(publisher, batch_size=batch_size)
        finally:
            connection.close()

    thread = threading.Thread(target=run)
    thread.start()
    assert publisher.holding.wait(10)
    return thread


def test_two_relays_never_publish_the_same_event(tenant: Tenant) -> None:
    first, second = order(tenant)["order_id"], order(tenant)["order_id"]
    a = BlockingPublisher()
    thread = _relay_in_background(a, batch_size=1)  # relay A holds the first order's event

    b = FakePublisher()
    assert relay_batch(b).published == 1  # relay B skips the locked row and takes the next one
    assert [m.key for m in b.sent] == [second]

    a.release.set()
    thread.join(10)
    assert [m.key for m in a.sent] == [first]
    assert not OutboxEvent.objects.filter(published_at__isnull=True).exists()


def test_a_relay_never_overtakes_another_on_the_same_order(tenant: Tenant) -> None:
    order_id = order(tenant)["order_id"]
    tenant.client.post(f"/v1/orders/{order_id}/cancel", {}, format="json")
    a = BlockingPublisher()
    thread = _relay_in_background(a, batch_size=1)  # A holds OrderConfirmed

    b = FakePublisher()
    assert relay_batch(b).published == 0  # OrderCancelled must wait for OrderConfirmed

    a.release.set()
    thread.join(10)
    assert relay_batch(b).published == 1
    sent = [m.headers for m in a.sent + b.sent]
    assert [dict(h)["type"] for h in sent] == [b"OrderConfirmed", b"OrderCancelled"]


def test_concurrent_redelivery_is_applied_once(
    tenant: Tenant, courier: CourierCtx, monkeypatch: pytest.MonkeyPatch
) -> None:
    order_id = order(tenant)["order_id"]
    event = dispatch_event("CourierAssigned", order_id, courier_id=courier.courier.id)
    original = services.assign_courier

    def slow(*args: Any) -> str:
        outcome = original(*args)
        time.sleep(0.5)
        return outcome

    monkeypatch.setattr(services, "assign_courier", slow)
    outcomes = _in_threads(
        lambda: deliver_dispatch_event(event), lambda: deliver_dispatch_event(event)
    )
    assert sorted(outcome for _, outcome in outcomes) == ["applied", "duplicate"]
    assert ProcessedEvent.objects.count() == 1
    assert Order.objects.get(pk=order_id).status == "assigned"
