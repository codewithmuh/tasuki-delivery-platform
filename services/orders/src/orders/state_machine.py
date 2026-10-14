"""The order lifecycle (docs/design.md §8) as one explicit table.

    confirmed   -> dispatching (OrderConfirmed published) | cancelled
    dispatching -> assigned (a courier accepted) | cancelled (merchant, or no courier in time)
    assigned    -> picked_up | dispatching (the courier dropped it) | cancelled
    picked_up   -> delivered (proof of delivery) | failed (customer unreachable)
    delivered, cancelled, failed: terminal

Every status change goes through `check_transition`. Anything not in the table is rejected (409), so
a new transition is a one-line, reviewable change here rather than a condition hidden in a view.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Final

from django.db import models

from core.errors import ConflictError


class OrderStatus(models.TextChoices):
    CONFIRMED = "confirmed"
    DISPATCHING = "dispatching"
    ASSIGNED = "assigned"
    PICKED_UP = "picked_up"
    DELIVERED = "delivered"
    CANCELLED = "cancelled"
    FAILED = "failed"


S = OrderStatus

TRANSITIONS: Final[Mapping[OrderStatus, frozenset[OrderStatus]]] = {
    S.CONFIRMED: frozenset({S.DISPATCHING, S.CANCELLED}),
    S.DISPATCHING: frozenset({S.ASSIGNED, S.CANCELLED}),
    S.ASSIGNED: frozenset({S.DISPATCHING, S.PICKED_UP, S.CANCELLED}),
    S.PICKED_UP: frozenset({S.DELIVERED, S.FAILED}),
    S.DELIVERED: frozenset(),
    S.CANCELLED: frozenset(),
    S.FAILED: frozenset(),
}

TERMINAL: Final = frozenset(status for status, targets in TRANSITIONS.items() if not targets)

# Status changes published on orders.events (contracts/events/order-event.v1). Dispatching and
# assigned are not: dispatch caused them, so it already knows.
PUBLISHED_EVENT_TYPE: Final[Mapping[OrderStatus, str]] = {
    S.CONFIRMED: "OrderConfirmed",
    S.CANCELLED: "OrderCancelled",
    S.PICKED_UP: "OrderPickedUp",
    S.DELIVERED: "OrderDelivered",
    S.FAILED: "OrderFailed",
}


class IllegalTransitionError(ConflictError):
    code = "illegal_transition"


def can_transition(current: OrderStatus | str, target: OrderStatus | str) -> bool:
    return OrderStatus(target) in TRANSITIONS[OrderStatus(current)]


def check_transition(current: OrderStatus | str, target: OrderStatus | str) -> None:
    if not can_transition(current, target):
        raise IllegalTransitionError(f"An order that is {current} cannot become {target}.")
