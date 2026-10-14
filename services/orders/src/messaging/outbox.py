"""Writing to the outbox. The relay (messaging.relay) is what publishes."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from django.db import transaction

from messaging.models import OutboxEvent


def enqueue(*, topic: str, key: str, event_id: str, payload: Mapping[str, Any]) -> OutboxEvent:
    """Add an event to the outbox, inside the caller's transaction.

    Refusing to run outside a transaction is the point: the event must commit or roll back together
    with the change it describes, so there is never an order without its event, or an event without
    its order.
    """
    if not transaction.get_connection().in_atomic_block:
        raise RuntimeError(
            "outbox events must be written inside the transaction that makes the change"
        )
    return OutboxEvent.objects.create(
        topic=topic, key=key, event_id=event_id, payload=dict(payload)
    )
