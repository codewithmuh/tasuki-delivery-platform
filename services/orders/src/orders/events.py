"""Builds order.event.v1 payloads (contracts/events/order-event.v1.schema.json).

Events carry coordinates for dispatch and nothing else about the customer: no name, phone or
address.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from orders.models import Order

SCHEMA = "order.event.v1"


def rfc3339(moment: datetime) -> str:
    return moment.astimezone(UTC).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def order_event(
    order: Order, event_type: str, *, event_id: str, occurred_at: datetime, reason: str = ""
) -> dict[str, Any]:
    store = order.store
    return {
        "schema": SCHEMA,
        "event_id": event_id,
        "type": event_type,
        "order_id": order.id,
        "merchant_id": order.merchant_id,
        "occurred_at": rfc3339(occurred_at),
        "order": {
            "store": {"id": store.id, "lat": store.lat, "lng": store.lng, "ward": store.ward},
            "dropoff": {
                "lat": order.dropoff_lat,
                "lng": order.dropoff_lng,
                "ward": order.dropoff_ward,
            },
            "ready_at": rfc3339(order.ready_at),
            "promised_at": rfc3339(order.promised_at),
            "fee_yen": order.fee_yen,
            "courier_id": order.courier_id,
        },
        "reason": reason or None,
    }
