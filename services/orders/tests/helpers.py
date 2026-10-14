"""Plain helpers shared by the tests (fixtures live in conftest.py)."""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from typing import Any

from rest_framework.test import APIClient

from accounts.models import Courier, Merchant, Store

# Distinctive personal data, so tests can prove it never leaks into events, logs or responses.
PII = {
    "address": "Jingumae 1-2-3 SENTINEL-ADDRESS",
    "customer_name": "Hanako SENTINEL-NAME",
    "customer_phone": "090-7731-4242",
}
SHIBUYA = {"lat": 35.6640, "lng": 139.6982, "ward": "shibuya"}
MEGURO = {"lat": 35.6415, "lng": 139.6982, "ward": "meguro"}  # 2.5 km due south of SHIBUYA
DISPATCH_ETA = {"eta_seconds": 1800, "pickup_s": 420, "travel_s": 780, "supply": "ok"}


@dataclass
class Tenant:
    merchant: Merchant
    api_key: str
    store: Store
    client: APIClient


@dataclass
class CourierCtx:
    courier: Courier
    token: str
    client: APIClient


def dropoff(**overrides: Any) -> dict[str, Any]:
    return {**MEGURO, **PII, **overrides}


def quote(tenant: Tenant, **dropoff_overrides: Any) -> dict[str, Any]:
    response = tenant.client.post(
        "/v1/quotes",
        {"store_id": tenant.store.id, "dropoff": dropoff(**dropoff_overrides)},
        format="json",
    )
    assert response.status_code == 201, response.content
    body: dict[str, Any] = response.json()
    return body


def post_order(tenant: Tenant, body: dict[str, Any], key: str | None = None) -> Any:
    return tenant.client.post(
        "/v1/orders", body, format="json", HTTP_IDEMPOTENCY_KEY=key or str(uuid.uuid4())
    )


def order(tenant: Tenant) -> dict[str, Any]:
    response = post_order(tenant, {"quote_id": quote(tenant)["quote_id"]})
    assert response.status_code == 201, response.content
    body: dict[str, Any] = response.json()
    return body


def dispatch_event(event_type: str, order_id: str, **fields: Any) -> dict[str, Any]:
    """A dispatch.event.v1 as the dispatch service sends it."""
    from django.utils import timezone

    from core.ids import new_event_id
    from orders.events import rfc3339

    return {
        "schema": "dispatch.event.v1",
        "event_id": new_event_id(),
        "type": event_type,
        "order_id": order_id,
        "courier_id": None,
        "offer_id": None,
        "ward": "shibuya",
        "occurred_at": rfc3339(timezone.now()),
        **fields,
    }


def deliver_dispatch_event(event: dict[str, Any]) -> tuple[str, str]:
    """Feed one event through the inbox, as the consumer would."""
    import json

    from messaging.consumer import process_event
    from orders.dispatch_events import HANDLERS

    return process_event(json.dumps(event).encode(), HANDLERS)


def assign(order_id: str, courier_id: str) -> None:
    event = dispatch_event("CourierAssigned", order_id, courier_id=courier_id)
    assert deliver_dispatch_event(event) == ("CourierAssigned", "applied")


class FakePublisher:
    """Stands in for Kafka. Acknowledges everything unless told which event IDs to drop."""

    def __init__(self, drop: set[str] | None = None, fail_all: bool = False) -> None:
        from messaging.relay import Message

        self.sent: list[Message] = []
        self.drop = drop or set()
        self.fail_all = fail_all

    def publish(self, messages: Any) -> set[str]:
        self.sent.extend(messages)
        if self.fail_all:
            return set()
        return {m.event_id for m in messages if m.event_id not in self.drop}

    def payloads(self) -> list[dict[str, Any]]:
        import json

        return [json.loads(m.value) for m in self.sent]
