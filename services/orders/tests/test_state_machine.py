import itertools

import pytest

from orders.state_machine import (
    PUBLISHED_EVENT_TYPE,
    TERMINAL,
    TRANSITIONS,
    IllegalTransitionError,
    OrderStatus,
    can_transition,
    check_transition,
)

S = OrderStatus

# docs/design.md §8, written out edge by edge so a change to the table must change this test too.
LEGAL = {
    (S.CONFIRMED, S.DISPATCHING),
    (S.CONFIRMED, S.CANCELLED),
    (S.DISPATCHING, S.ASSIGNED),
    (S.DISPATCHING, S.CANCELLED),
    (S.ASSIGNED, S.DISPATCHING),
    (S.ASSIGNED, S.PICKED_UP),
    (S.ASSIGNED, S.CANCELLED),
    (S.PICKED_UP, S.DELIVERED),
    (S.PICKED_UP, S.FAILED),
}


@pytest.mark.parametrize(("current", "target"), list(itertools.product(S, S)))
def test_every_pair_of_statuses(current: OrderStatus, target: OrderStatus) -> None:
    legal = (current, target) in LEGAL
    assert can_transition(current, target) is legal
    if legal:
        check_transition(current, target)
    else:
        with pytest.raises(IllegalTransitionError) as excinfo:
            check_transition(current, target)
        assert excinfo.value.status == 409
        assert excinfo.value.code == "illegal_transition"


def test_table_covers_every_status() -> None:
    assert set(TRANSITIONS) == set(S)


def test_terminal_statuses() -> None:
    assert {S.DELIVERED, S.CANCELLED, S.FAILED} == TERMINAL


def test_cannot_cancel_after_pickup() -> None:
    assert not can_transition(S.PICKED_UP, S.CANCELLED)


def test_published_event_types_match_the_contract() -> None:
    # contracts/events/order-event.v1.schema.json, property "type"
    assert set(PUBLISHED_EVENT_TYPE.values()) == {
        "OrderConfirmed",
        "OrderCancelled",
        "OrderPickedUp",
        "OrderDelivered",
        "OrderFailed",
    }


def test_accepts_plain_strings() -> None:
    assert can_transition("confirmed", "dispatching")
