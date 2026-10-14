"""Outbox (events waiting to be published) and inbox (events already handled)."""

from __future__ import annotations

from django.db import models
from django.db.models import Q
from django.utils import timezone

from core import ids


class OutboxEvent(models.Model):
    """An event written in the same transaction as the change it describes; the relay publishes it.

    `id` is the publish order. Events with the same `key` (an order ID) are published oldest first.
    """

    id = models.BigAutoField(primary_key=True)
    event_id = models.CharField(max_length=ids.ID_LENGTH, unique=True)
    topic = models.CharField(max_length=100)
    key = models.CharField(max_length=100)
    payload = models.JSONField()
    created_at = models.DateTimeField(default=timezone.now)
    published_at = models.DateTimeField(null=True, blank=True)

    class Meta:
        indexes = [
            # The relay's scan: pending events in publish order. Partial, so it stays as small as
            # the backlog.
            models.Index(
                fields=["id"], name="outbox_pending_idx", condition=Q(published_at__isnull=True)
            ),
            # "Is there an older pending event for this key?" (keeps per-order ordering across
            # relays).
            models.Index(
                fields=["key", "id"],
                name="outbox_pending_key_idx",
                condition=Q(published_at__isnull=True),
            ),
            models.Index(
                fields=["published_at"],
                name="outbox_published_at_idx",
                condition=Q(published_at__isnull=False),
            ),
        ]

    def __str__(self) -> str:
        return self.event_id


class ProcessedEvent(models.Model):
    """The inbox: IDs of consumed events, so a redelivered event is recognised and skipped."""

    event_id = models.CharField(primary_key=True, max_length=ids.ID_LENGTH)
    processed_at = models.DateTimeField(default=timezone.now, db_index=True)

    def __str__(self) -> str:
        return self.event_id
