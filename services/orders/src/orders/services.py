"""The order use cases. Views translate HTTP; everything that changes an order happens here.

Rules every status change follows (see `_transition`):
- the order row is locked (SELECT … FOR UPDATE) for the rest of the transaction, so two changes to
  one order run one after the other;
- the change is checked against the state machine table;
- one history row is appended, and an outbox event is written if the new status is published; all
  three commit together or not at all.
"""

from __future__ import annotations

import logging
from collections.abc import Sequence
from dataclasses import dataclass
from datetime import datetime, timedelta

from django.conf import settings
from django.db import transaction
from django.db.models import QuerySet
from django.utils import timezone
from django.utils.crypto import salted_hmac

from accounts.models import Courier, Merchant, Store
from core import ids
from core.errors import ConflictError, NotFoundError, UnprocessableError
from core.geo import haversine_m
from core.metrics import ORDERS_CREATED, QUOTES_CREATED, STATUS_TRANSITIONS
from messaging import outbox
from messaging.models import OutboxEvent
from orders import eta, pricing
from orders.events import order_event
from orders.models import Order, OrderEvent, Quote
from orders.state_machine import PUBLISHED_EVENT_TYPE, OrderStatus, check_transition

log = logging.getLogger("orders.services")


class QuoteNotFoundError(NotFoundError):
    code = "quote_not_found"
    default_message = "No such quote."


class QuoteExpiredError(ConflictError):
    code = "quote_expired"
    default_message = "The quote has expired. Ask for a new one."


class QuoteAlreadyUsedError(ConflictError):
    code = "quote_already_used"
    default_message = "An order was already created from this quote."


class DropoffMismatchError(UnprocessableError):
    code = "dropoff_mismatch"
    default_message = "The drop-off differs from the one that was quoted."


class ProofTooFarError(UnprocessableError):
    code = "proof_too_far"
    default_message = "The delivery position is too far from the drop-off."


class OrderNotFoundError(NotFoundError):
    default_message = "No such order."


@dataclass(frozen=True)
class Dropoff:
    lat: float
    lng: float
    ward: str
    address: str
    customer_name: str
    customer_phone: str

    def fingerprint(self) -> str:
        """HMAC (keyed with SECRET_KEY) of the whole drop-off.

        Keyed, because a plain hash of a phone number can be reversed by trying every number.
        """
        value = "\x1f".join(
            [
                repr(self.lat),
                repr(self.lng),
                self.ward,
                self.address,
                self.customer_name,
                self.customer_phone,
            ]
        )
        return salted_hmac("orders.quote.dropoff", value, algorithm="sha256").hexdigest()


def actor(kind: str, principal_id: str | None = None) -> str:
    return f"{kind}:{principal_id}" if principal_id else kind


# ---- Quotes --------------------------------------------------------------------------------------


def create_quote(merchant: Merchant, store: Store, dropoff: Dropoff) -> Quote:
    """Price and ETA. Calls dispatch, so it runs outside any transaction (no lock over network)."""
    distance_m = round(haversine_m(store.lat, store.lng, dropoff.lat, dropoff.lng))
    fee = pricing.delivery_fee(distance_m)
    estimate = eta.estimate_eta(store, dropoff.lat, dropoff.lng, distance_m)
    now = timezone.now()
    quote = Quote.objects.create(
        merchant=merchant,
        store=store,
        dropoff_lat=dropoff.lat,
        dropoff_lng=dropoff.lng,
        dropoff_ward=dropoff.ward,
        dropoff_address=dropoff.address,
        customer_name=dropoff.customer_name,
        customer_phone=dropoff.customer_phone,
        dropoff_hash=dropoff.fingerprint(),
        distance_m=distance_m,
        fee_excl_tax_yen=fee.excl_tax_yen,
        tax_yen=fee.tax_yen,
        fee_yen=fee.total_yen,
        eta_seconds=estimate.seconds,
        eta_source=estimate.source,
        created_at=now,
        expires_at=now + timedelta(seconds=settings.QUOTE_TTL_SECONDS),
    )
    QUOTES_CREATED.labels(estimate.source).inc()
    return quote


# ---- Orders --------------------------------------------------------------------------------------


def create_order(merchant: Merchant, quote_id: str, dropoff: Dropoff | None = None) -> Order:
    """Turn a quote into a confirmed order. Runs inside idempotency.run_once's transaction."""
    now = timezone.now()
    quote = (
        Quote.objects.select_for_update()  # serialises two orders racing for the same quote
        .select_related("store")
        .filter(merchant=merchant, pk=quote_id)
        .first()
    )
    if quote is None:
        raise QuoteNotFoundError()
    if quote.is_expired(now):
        raise QuoteExpiredError()
    if dropoff is not None and dropoff.fingerprint() != quote.dropoff_hash:
        raise DropoffMismatchError()
    if Order.objects.filter(quote=quote).exists():
        raise QuoteAlreadyUsedError()

    store = quote.store
    order = Order(
        merchant=merchant,
        store=store,
        quote=quote,
        status=OrderStatus.CONFIRMED,
        dropoff_lat=quote.dropoff_lat,
        dropoff_lng=quote.dropoff_lng,
        dropoff_ward=quote.dropoff_ward,
        dropoff_address=quote.dropoff_address,
        customer_name=quote.customer_name,
        customer_phone=quote.customer_phone,
        distance_m=quote.distance_m,
        fee_excl_tax_yen=quote.fee_excl_tax_yen,
        tax_yen=quote.tax_yen,
        fee_yen=quote.fee_yen,
        eta_seconds=quote.eta_seconds,
        # The promise is fixed now, at confirmation: the quoted ETA counted from this moment.
        promised_at=now + timedelta(seconds=quote.eta_seconds),
        ready_at=now + timedelta(minutes=store.prep_minutes),
        created_at=now,
        updated_at=now,
    )
    # The quote row lock above makes the exists() check reliable. The unique OneToOne on quote is
    # the backstop: if that ever failed, the insert raises (500) rather than create a second order.
    order.save(force_insert=True)
    _record(order, previous="", actor_=actor("merchant", merchant.id), now=now)
    transaction.on_commit(ORDERS_CREATED.inc)
    log.info("order confirmed", extra={"order_id": order.id, "merchant_id": merchant.id})
    return order


def cancel_order(merchant: Merchant, order_id: str, reason: str) -> Order:
    """Merchant cancels. Allowed before pickup (the state machine decides)."""
    with transaction.atomic():
        order = _lock(Order.objects.filter(merchant=merchant), order_id)
        _transition(order, OrderStatus.CANCELLED, actor("merchant", merchant.id), reason=reason)
    return order


def pick_up(courier: Courier, order_id: str) -> Order:
    with transaction.atomic():
        order = _lock(Order.objects.filter(courier=courier), order_id)
        check_transition(order.status, OrderStatus.PICKED_UP)
        order.picked_up_at = timezone.now()
        _transition(order, OrderStatus.PICKED_UP, actor("courier", courier.id))
    return order


def deliver(courier: Courier, order_id: str, lat: float, lng: float) -> Order:
    """Proof of delivery: the courier must be within DELIVERY_PROOF_RADIUS_M of the drop-off."""
    with transaction.atomic():
        order = _lock(Order.objects.filter(courier=courier), order_id)
        check_transition(order.status, OrderStatus.DELIVERED)
        distance = haversine_m(lat, lng, order.dropoff_lat, order.dropoff_lng)
        if distance > settings.DELIVERY_PROOF_RADIUS_M:
            raise ProofTooFarError(
                f"The delivery position is {round(distance)} m from the drop-off "
                f"(at most {settings.DELIVERY_PROOF_RADIUS_M} m)."
            )
        order.delivered_at = timezone.now()
        order.delivery_lat, order.delivery_lng = lat, lng
        _transition(order, OrderStatus.DELIVERED, actor("courier", courier.id))
    return order


def fail_delivery(courier: Courier, order_id: str, reason: str) -> Order:
    """The courier has the order but cannot hand it over (customer unreachable, …)."""
    with transaction.atomic():
        order = _lock(Order.objects.filter(courier=courier), order_id)
        _transition(order, OrderStatus.FAILED, actor("courier", courier.id), reason=reason)
    return order


# ---- Reactions to events -------------------------------------------------------------------------


def on_events_published(events: Sequence[OutboxEvent]) -> None:
    """Relay hook: runs inside the relay's transaction, after Kafka acknowledged `events`."""
    confirmed = [event.key for event in events if event.payload.get("type") == "OrderConfirmed"]
    if confirmed:
        mark_dispatching(confirmed)


def mark_dispatching(order_ids: list[str]) -> None:
    """OrderConfirmed reached Kafka, so dispatch sees these orders. Runs in the relay transaction.

    Only orders still `confirmed` move: one cancelled meanwhile, or already assigned (dispatch can
    be faster than our commit), is left alone.
    """
    for order in Order.objects.select_for_update().filter(
        pk__in=order_ids, status=OrderStatus.CONFIRMED
    ):
        _transition(order, OrderStatus.DISPATCHING, actor("relay"))


def assign_courier(order_id: str, courier_id: str) -> str:
    """CourierAssigned from dispatch. Returns an outcome label: applied / ignored."""
    order = Order.objects.select_for_update().filter(pk=order_id).first()
    courier = Courier.objects.filter(pk=courier_id).first()
    if order is None or courier is None:
        log.warning(
            "CourierAssigned for an unknown order or courier",
            extra={"order_id": order_id, "courier_id": courier_id},
        )
        return "ignored"
    if order.status == OrderStatus.ASSIGNED and order.courier_id == courier_id:
        return "ignored"  # already applied (e.g. the same assignment under a new event ID)
    if order.status == OrderStatus.CONFIRMED:
        # Dispatch can assign before the relay's commit moved the order on: catch up first.
        _transition(order, OrderStatus.DISPATCHING, actor("dispatch"))
    if order.status != OrderStatus.DISPATCHING:
        # Cancelled meanwhile, or held by another courier. Dispatch learns from OrderCancelled.
        log.warning(
            "CourierAssigned for an order that is not dispatching",
            extra={"order_id": order_id, "status": order.status},
        )
        return "ignored"
    order.courier = courier
    _transition(order, OrderStatus.ASSIGNED, actor("dispatch"))
    return "applied"


def no_courier_available(order_id: str) -> str:
    """NoCourierAvailable from dispatch: keep waiting, until the order is too old; then cancel it.

    Dispatch keeps retrying a ward's pending orders and reports each miss. Before the deadline the
    order stays `dispatching` (supply often recovers within minutes). After
    NO_COURIER_CANCEL_AFTER_SECONDS it is cancelled with reason `no_courier_available`, which
    publishes OrderCancelled to dispatch too.
    """
    order = Order.objects.select_for_update().filter(pk=order_id).first()
    if order is None or order.status not in (OrderStatus.CONFIRMED, OrderStatus.DISPATCHING):
        return "ignored"
    waited = (timezone.now() - order.created_at).total_seconds()
    if waited < settings.NO_COURIER_CANCEL_AFTER_SECONDS:
        return "waiting"
    _transition(order, OrderStatus.CANCELLED, actor("dispatch"), reason="no_courier_available")
    return "cancelled"


# ---- Internals -----------------------------------------------------------------------------------


def _lock(orders: QuerySet[Order], order_id: str) -> Order:
    order = orders.select_for_update().filter(pk=order_id).first()
    if order is None:
        raise OrderNotFoundError()  # also for other merchants' orders: never reveal that they exist
    return order


def _transition(order: Order, target: OrderStatus, actor_: str, *, reason: str = "") -> None:
    """Move a locked order to `target`: check, save, append history, enqueue its event if any."""
    check_transition(order.status, target)
    previous = order.status
    now = timezone.now()
    order.status = target
    order.status_reason = reason
    order.updated_at = now
    order.save()
    _record(order, previous=previous, actor_=actor_, now=now, reason=reason)


def _record(order: Order, *, previous: str, actor_: str, now: datetime, reason: str = "") -> None:
    OrderEvent.objects.create(
        order=order,
        from_status=previous,
        to_status=order.status,
        actor=actor_,
        reason=reason,
        occurred_at=now,
    )
    event_type = PUBLISHED_EVENT_TYPE.get(OrderStatus(order.status))
    if event_type is not None:
        event_id = ids.new_event_id()
        outbox.enqueue(
            topic=settings.KAFKA_ORDERS_TOPIC,
            key=order.id,
            event_id=event_id,
            payload=order_event(
                order, event_type, event_id=event_id, occurred_at=now, reason=reason
            ),
        )
    status = order.status
    transaction.on_commit(lambda: STATUS_TRANSITIONS.labels(status).inc())
