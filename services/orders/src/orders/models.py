"""Quotes, orders, their append-only status history, and idempotency records.

Money is integer yen (JPY has no minor unit). Customer name, phone and address text are encrypted
per field and never leave this service except to the people who need them.
"""

from __future__ import annotations

from datetime import datetime, timedelta

from django.db import models
from django.db.models import F, Q
from django.utils import timezone

from accounts.models import Courier, Merchant, Store
from core import ids
from core.crypto import EncryptedTextField
from orders.state_machine import OrderStatus


class Quote(models.Model):
    """A price and ETA for one delivery, valid for QUOTE_TTL_SECONDS (2 minutes).

    Short expiry stops a merchant locking in a pre-surge price during a surge. Quotes hold the
    drop-off (PII encrypted) only until purge_expired deletes them, 24 hours later.
    """

    class EtaSource(models.TextChoices):
        DISPATCH = "dispatch"
        FALLBACK = "fallback"

    id = models.CharField(
        primary_key=True, max_length=ids.ID_LENGTH, default=ids.new_quote_id, editable=False
    )
    merchant = models.ForeignKey(Merchant, on_delete=models.CASCADE, related_name="quotes")
    store = models.ForeignKey(Store, on_delete=models.CASCADE, related_name="quotes")
    dropoff_lat = models.FloatField()
    dropoff_lng = models.FloatField()
    dropoff_ward = models.CharField(max_length=20)
    dropoff_address = EncryptedTextField()
    customer_name = EncryptedTextField()
    customer_phone = EncryptedTextField()
    # Keyed hash of the whole drop-off: an order can prove it is for the drop-off that was priced.
    dropoff_hash = models.CharField(max_length=64)
    distance_m = models.PositiveIntegerField()
    fee_excl_tax_yen = models.BigIntegerField()
    tax_yen = models.BigIntegerField()
    fee_yen = models.BigIntegerField()
    eta_seconds = models.PositiveIntegerField()
    eta_source = models.CharField(max_length=10, choices=EtaSource.choices)
    created_at = models.DateTimeField(default=timezone.now, db_index=True)
    expires_at = models.DateTimeField()

    class Meta:
        constraints = [
            models.CheckConstraint(
                condition=Q(fee_yen=F("fee_excl_tax_yen") + F("tax_yen")) & Q(tax_yen__gte=0),
                name="quote_fee_adds_up",
            )
        ]

    def __str__(self) -> str:
        return self.id

    @property
    def promised_at(self) -> datetime:
        return self.created_at + timedelta(seconds=self.eta_seconds)

    def is_expired(self, now: datetime) -> bool:
        return now >= self.expires_at


class Order(models.Model):
    id = models.CharField(
        primary_key=True, max_length=ids.ID_LENGTH, default=ids.new_order_id, editable=False
    )
    merchant = models.ForeignKey(Merchant, on_delete=models.PROTECT, related_name="orders")
    store = models.ForeignKey(Store, on_delete=models.PROTECT, related_name="orders")
    # One order per quote, enforced by the database (a OneToOne is a unique foreign key). The link
    # is cleared when the quote is purged; the order keeps its own copy of everything that was
    # agreed.
    quote = models.OneToOneField(
        Quote, null=True, blank=True, on_delete=models.SET_NULL, related_name="order"
    )
    status = models.CharField(
        max_length=16, choices=OrderStatus.choices, default=OrderStatus.CONFIRMED
    )
    status_reason = models.CharField(max_length=40, blank=True, default="")

    dropoff_lat = models.FloatField()
    dropoff_lng = models.FloatField()
    dropoff_ward = models.CharField(max_length=20)
    dropoff_address = EncryptedTextField()
    customer_name = EncryptedTextField()
    customer_phone = EncryptedTextField()

    distance_m = models.PositiveIntegerField()
    fee_excl_tax_yen = models.BigIntegerField()
    tax_yen = models.BigIntegerField()
    fee_yen = models.BigIntegerField()
    eta_seconds = models.PositiveIntegerField()
    ready_at = models.DateTimeField()  # when the store expects the order to be ready for pickup
    promised_at = models.DateTimeField()  # the ETA promised at confirmation (the ETA-accuracy SLO)

    courier = models.ForeignKey(
        Courier, null=True, blank=True, on_delete=models.PROTECT, related_name="orders"
    )
    picked_up_at = models.DateTimeField(null=True, blank=True)
    delivered_at = models.DateTimeField(null=True, blank=True)
    # Proof of delivery: where the courier was when they marked the order delivered.
    delivery_lat = models.FloatField(null=True, blank=True)
    delivery_lng = models.FloatField(null=True, blank=True)

    created_at = models.DateTimeField(default=timezone.now)
    updated_at = models.DateTimeField(default=timezone.now)

    class Meta:
        indexes = [
            models.Index(fields=["merchant", "-created_at"], name="order_merchant_recent_idx"),
            models.Index(
                fields=["status", "created_at"],
                name="order_open_idx",
                condition=Q(status__in=["confirmed", "dispatching", "assigned", "picked_up"]),
            ),
        ]
        constraints = [
            models.CheckConstraint(
                condition=Q(status__in=OrderStatus.values), name="order_status_valid"
            ),
            models.CheckConstraint(
                condition=Q(fee_yen=F("fee_excl_tax_yen") + F("tax_yen")) & Q(tax_yen__gte=0),
                name="order_fee_adds_up",
            ),
        ]

    def __str__(self) -> str:
        return self.id


class OrderEvent(models.Model):
    """Append-only status history: one row per status change, never updated or deleted."""

    id = models.BigAutoField(primary_key=True)
    order = models.ForeignKey(Order, on_delete=models.CASCADE, related_name="history")
    from_status = models.CharField(max_length=16, blank=True, default="")  # "" for creation
    to_status = models.CharField(max_length=16, choices=OrderStatus.choices)
    actor = models.CharField(max_length=64)  # e.g. "merchant:mer_…", "courier:cou_…", "dispatch"
    reason = models.CharField(max_length=40, blank=True, default="")
    occurred_at = models.DateTimeField(default=timezone.now)

    class Meta:
        ordering = ["id"]

    def __str__(self) -> str:
        return f"{self.order_id}: {self.from_status or '-'} -> {self.to_status}"

    def save(self, *args: object, **kwargs: object) -> None:
        if not self._state.adding:
            raise RuntimeError("order history is append-only")
        super().save(*args, **kwargs)  # type: ignore[arg-type]


class IdempotencyRecord(models.Model):
    """The stored response to a POST /v1/orders, by (merchant, Idempotency-Key), kept 24 hours.

    The unique constraint is what makes concurrent duplicates safe; see orders.idempotency.
    """

    id = models.BigAutoField(primary_key=True)
    merchant = models.ForeignKey(Merchant, on_delete=models.CASCADE, related_name="+")
    key = models.CharField(max_length=255)
    request_hash = models.CharField(max_length=64)
    # Null only inside the transaction that claims the key; committed rows always have a response.
    response_status = models.PositiveSmallIntegerField(null=True)
    response_body = models.JSONField(null=True)
    created_at = models.DateTimeField(default=timezone.now, db_index=True)

    class Meta:
        constraints = [
            models.UniqueConstraint(fields=["merchant", "key"], name="idempotency_key_per_merchant")
        ]

    def __str__(self) -> str:
        return f"{self.merchant_id}:{self.key}"
