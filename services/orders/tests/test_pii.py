"""Customer name, phone, address: encrypted at rest; never in events, logs or responses."""

from __future__ import annotations

import json
import logging

import pytest
from django.db import connection

from messaging.models import OutboxEvent
from messaging.relay import relay_batch
from orders.models import IdempotencyRecord, Order
from orders.services import on_events_published
from tests.helpers import MEGURO, PII, CourierCtx, FakePublisher, Tenant, assign, order, quote


@pytest.fixture
def full_lifecycle(tenant: Tenant, courier: CourierCtx, caplog: pytest.LogCaptureFixture) -> str:
    """Quote, order, assign, pick up, deliver and publish, capturing every log line."""
    caplog.set_level(logging.DEBUG)
    created = order(tenant)
    order_id: str = created["order_id"]
    tenant.client.get(f"/v1/orders/{order_id}")
    assign(order_id, courier.courier.id)
    courier.client.post(f"/v1/courier/orders/{order_id}/pickup")
    courier.client.post(f"/v1/courier/orders/{order_id}/deliver", MEGURO, format="json")
    relay_batch(FakePublisher(), on_published=on_events_published)
    return order_id


def _assert_no_pii(text: str) -> None:
    for value in PII.values():
        assert value not in text
    for fragment in ("SENTINEL", "7731"):
        assert fragment not in text


def test_events_carry_no_personal_data(full_lifecycle: str) -> None:
    payloads = json.dumps([e.payload for e in OutboxEvent.objects.all()])
    _assert_no_pii(payloads)


def test_logs_carry_no_personal_data(full_lifecycle: str, caplog: pytest.LogCaptureFixture) -> None:
    records = caplog.get_records("setup") + caplog.records
    assert records  # the flow did log
    for record in records:
        text = record.getMessage() + json.dumps(vars(record), default=str)
        _assert_no_pii(text)
        # Not even the drop-off coordinates (they are in events, which is their job, but not in
        # logs).
        assert str(MEGURO["lat"]) not in text


def test_responses_and_stored_replays_carry_no_personal_data(
    tenant: Tenant, full_lifecycle: str
) -> None:
    _assert_no_pii(tenant.client.get(f"/v1/orders/{full_lifecycle}").content.decode())
    _assert_no_pii(json.dumps([r.response_body for r in IdempotencyRecord.objects.all()]))
    _assert_no_pii(json.dumps(quote(tenant)))


def test_order_pii_is_encrypted_at_rest(full_lifecycle: str) -> None:
    with connection.cursor() as cursor:
        cursor.execute(
            "SELECT dropoff_address, customer_name, customer_phone FROM orders_order WHERE id = %s",
            [full_lifecycle],
        )
        raw = cursor.fetchone()
    assert raw is not None
    _assert_no_pii(" ".join(raw))
    row = Order.objects.get(pk=full_lifecycle)
    assert (row.dropoff_address, row.customer_name, row.customer_phone) == tuple(PII.values())


def test_field_key_rotation(tenant: Tenant, settings: object) -> None:
    from cryptography.fernet import Fernet

    order_id = order(tenant)["order_id"]
    old_key = settings.ORDERS_FIELD_KEYS[0]  # type: ignore[attr-defined]
    # A new primary key encrypts new writes; the old key still decrypts existing rows.
    settings.ORDERS_FIELD_KEYS = [Fernet.generate_key().decode(), old_key]  # type: ignore[attr-defined]
    assert Order.objects.get(pk=order_id).customer_name == PII["customer_name"]
