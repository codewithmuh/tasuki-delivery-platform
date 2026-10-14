"""The outbox relay: publish pending outbox events to Kafka, then mark them published.

Guarantees:
- At-least-once. An event is marked published only after Kafka acknowledged it. A crash between the
  acknowledgement and the commit re-sends it, so consumers dedupe on `event_id`.
- Safe with several replicas. Each takes a batch with `FOR UPDATE SKIP LOCKED`, so two relays never
  hold the same row; one that dies releases its rows when its transaction aborts.
- Per-order order. A relay only takes the oldest pending event of each key. While a replica holds
  event 1 of an order, no other replica can take event 2 of that order, so events are never
  reordered.
"""

from __future__ import annotations

import json
import logging
import threading
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from typing import Protocol

from django.db import close_old_connections, connection, transaction
from django.db.models import Count, Exists, Min, OuterRef, QuerySet
from django.utils import timezone

from core.metrics import OUTBOX_LAG, OUTBOX_PENDING, OUTBOX_PUBLISH_FAILURES, OUTBOX_PUBLISHED
from messaging.models import OutboxEvent

log = logging.getLogger("orders.relay")


@dataclass(frozen=True)
class Message:
    topic: str
    key: str
    value: bytes
    event_id: str
    headers: tuple[tuple[str, bytes], ...]


class Publisher(Protocol):
    def publish(self, messages: Sequence[Message]) -> set[str]:
        """Send the messages and wait; return the event IDs the broker acknowledged."""
        ...


# Called inside the relay's transaction with the events that were just published.
OnPublished = Callable[[Sequence[OutboxEvent]], None]


@dataclass(frozen=True)
class BatchResult:
    published: int
    failed: int


def pending_heads() -> QuerySet[OutboxEvent]:
    """Pending events with no older pending event for the same key, oldest first."""
    older_pending = OutboxEvent.objects.filter(
        key=OuterRef("key"), published_at__isnull=True, id__lt=OuterRef("id")
    )
    return (
        OutboxEvent.objects.filter(published_at__isnull=True)
        .filter(~Exists(older_pending))
        .order_by("id")
    )


def to_message(event: OutboxEvent) -> Message:
    headers = (
        ("event_id", event.event_id.encode()),
        ("schema", str(event.payload.get("schema", "")).encode()),
        ("type", str(event.payload.get("type", "")).encode()),
        ("content-type", b"application/json"),
    )
    return Message(
        topic=event.topic,
        key=event.key,
        value=json.dumps(event.payload, separators=(",", ":"), ensure_ascii=False).encode(),
        event_id=event.event_id,
        headers=headers,
    )


def relay_batch(
    publisher: Publisher, *, batch_size: int = 100, on_published: OnPublished | None = None
) -> BatchResult:
    """Publish one batch. Its rows stay locked until Kafka has answered and the commit runs."""
    with transaction.atomic():
        batch = list(pending_heads().select_for_update(skip_locked=True)[:batch_size])
        if not batch:
            return BatchResult(published=0, failed=0)
        acknowledged = publisher.publish([to_message(event) for event in batch])
        published = [event for event in batch if event.event_id in acknowledged]
        if published:
            OutboxEvent.objects.filter(pk__in=[event.pk for event in published]).update(
                published_at=timezone.now()
            )
            if on_published is not None:
                on_published(published)
    failed = len(batch) - len(published)
    OUTBOX_PUBLISHED.inc(len(published))
    if failed:
        OUTBOX_PUBLISH_FAILURES.inc(failed)
        log.warning("outbox events not acknowledged; will retry", extra={"failed": failed})
    return BatchResult(published=len(published), failed=failed)


def record_lag() -> None:
    """Update the backlog gauges: how many events wait, and how old the oldest one is."""
    stats = OutboxEvent.objects.filter(published_at__isnull=True).aggregate(
        oldest=Min("created_at"), pending=Count("id")
    )
    OUTBOX_PENDING.set(stats["pending"])
    oldest = stats["oldest"]
    OUTBOX_LAG.set((timezone.now() - oldest).total_seconds() if oldest is not None else 0)


def run_relay(
    publisher: Publisher,
    stop: threading.Event,
    *,
    batch_size: int = 100,
    poll_interval_s: float = 0.5,
    max_backoff_s: float = 30.0,
    on_published: OnPublished | None = None,
    heartbeat: Callable[[], None] = lambda: None,
) -> None:
    """Relay until `stop` is set. Survives database and Kafka outages by backing off."""
    backoff = poll_interval_s
    while not stop.is_set():
        close_old_connections()  # long-running process: drop connections Django considers stale
        try:
            record_lag()  # measured before publishing, so it is current even while Kafka is down
            result = relay_batch(publisher, batch_size=batch_size, on_published=on_published)
        except Exception:
            log.exception("outbox relay iteration failed")
            connection.close()  # a broken connection is replaced on the next query
            result = BatchResult(published=0, failed=1)
        heartbeat()
        if result.failed and not result.published:
            stop.wait(backoff)  # Kafka or the database is down: back off exponentially
            backoff = min(backoff * 2, max_backoff_s)
            continue
        backoff = poll_interval_s
        if result.published < batch_size:
            stop.wait(poll_interval_s)  # caught up: poll again shortly
