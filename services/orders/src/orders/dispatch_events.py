"""Handlers for dispatch.events (contracts/events/dispatch-event.v1.schema.json).

Run by messaging.consumer inside the inbox transaction, so each event's effects happen exactly once.
Offer events (OfferSent, OfferAccepted, ...) are dispatch's business; orders only reacts to the
outcome.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from messaging.consumer import Handler
from orders import services


def on_courier_assigned(event: dict[str, Any]) -> str:
    order_id, courier_id = event.get("order_id"), event.get("courier_id")
    if not isinstance(order_id, str) or not isinstance(courier_id, str):
        return "invalid"
    return services.assign_courier(order_id, courier_id)


def on_no_courier_available(event: dict[str, Any]) -> str:
    order_id = event.get("order_id")
    if not isinstance(order_id, str):
        return "invalid"
    return services.no_courier_available(order_id)


HANDLERS: Mapping[str, Handler] = {
    "CourierAssigned": on_courier_assigned,
    "NoCourierAvailable": on_no_courier_available,
}
