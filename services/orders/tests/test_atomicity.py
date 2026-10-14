"""An order, its history, its outbox event and its idempotency record: all committed, or none."""

from __future__ import annotations

from typing import Any

import pytest
from django.db import transaction

from messaging import outbox
from messaging.models import OutboxEvent
from orders.models import IdempotencyRecord, Order, OrderEvent
from tests.helpers import Tenant, post_order, quote


def test_a_failing_outbox_write_leaves_nothing_behind(
    tenant: Tenant, monkeypatch: pytest.MonkeyPatch
) -> None:
    q = quote(tenant)

    def broken_enqueue(**kwargs: Any) -> None:
        raise RuntimeError("disk full")

    monkeypatch.setattr("orders.services.outbox.enqueue", broken_enqueue)
    tenant.client.raise_request_exception = False
    response = post_order(tenant, {"quote_id": q["quote_id"]}, key="atomic")
    assert response.status_code == 500
    assert not Order.objects.exists()
    assert not OrderEvent.objects.exists()
    assert not OutboxEvent.objects.exists()
    assert not IdempotencyRecord.objects.exists()

    # The retry, with the same key, succeeds: nothing half-done blocks it.
    monkeypatch.undo()
    assert post_order(tenant, {"quote_id": q["quote_id"]}, key="atomic").status_code == 201
    assert Order.objects.count() == OutboxEvent.objects.count() == 1


def test_a_failing_history_write_rolls_back_the_cancel(
    tenant: Tenant, monkeypatch: pytest.MonkeyPatch
) -> None:
    q = quote(tenant)
    order_id = post_order(tenant, {"quote_id": q["quote_id"]}).json()["order_id"]

    def broken_create(*args: Any, **kwargs: Any) -> None:
        raise RuntimeError("history table locked")

    monkeypatch.setattr(OrderEvent.objects, "create", broken_create)
    tenant.client.raise_request_exception = False
    response = tenant.client.post(f"/v1/orders/{order_id}/cancel", {}, format="json")
    assert response.status_code == 500
    assert Order.objects.get(pk=order_id).status == "confirmed"
    assert OutboxEvent.objects.filter(key=order_id).count() == 1  # only OrderConfirmed


@pytest.mark.django_db(transaction=True)
def test_outbox_refuses_writes_outside_a_transaction() -> None:
    with pytest.raises(RuntimeError, match="inside the transaction"):
        outbox.enqueue(topic="t", key="k", event_id="evt_x", payload={})
    with transaction.atomic():
        assert outbox.enqueue(topic="t", key="k", event_id="evt_x", payload={}).pk
