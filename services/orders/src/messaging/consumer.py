"""The idempotent inbox: handle each consumed event once, however often Kafka delivers it.

Kafka delivers at least once: after a rebalance or a crash before the offset commit, the same event
arrives again. The event's ID is recorded in ProcessedEvent in the same transaction as its effects,
so a redelivery finds the ID and does nothing.
"""

from __future__ import annotations

import json
import logging
import threading
from collections.abc import Callable, Mapping
from typing import Any, Protocol

from django.db import IntegrityError, close_old_connections, connection, transaction

from core.metrics import DISPATCH_EVENTS
from messaging.models import ProcessedEvent

log = logging.getLogger("orders.consumer")

# Applies one event's effects inside the inbox transaction; returns an outcome label for metrics.
Handler = Callable[[dict[str, Any]], str]


def process_event(raw: bytes | None, handlers: Mapping[str, Handler]) -> tuple[str, str]:
    """Handle one message. Returns (event type, outcome) for metrics and logs."""
    try:
        event = json.loads(raw or b"")
        event_id, event_type = event["event_id"], event["type"]
        if not isinstance(event_id, str) or not isinstance(event_type, str):
            raise TypeError("event_id and type must be strings")
    except (ValueError, KeyError, TypeError):
        # A poison message would block the partition forever if retried; skip it and say so loudly.
        # (A dead-letter topic is the production answer: see the service README.)
        log.error("unreadable event skipped")
        return "unknown", "invalid"

    handler = handlers.get(event_type)
    if handler is None:
        return event_type, "ignored"  # a type this service doesn't act on; nothing to remember

    with transaction.atomic():
        try:
            # Claim the event ID first, in a savepoint. If another consumer is handling the same
            # event right now, this insert waits for it, then fails: the event has been handled.
            with transaction.atomic():
                ProcessedEvent.objects.create(event_id=event_id)
        except IntegrityError:
            return event_type, "duplicate"
        # Any exception from here rolls back the claim and the effects together; the loop retries.
        return event_type, handler(event)


class KafkaMessage(Protocol):
    def error(self) -> Any: ...
    def value(self) -> bytes | None: ...
    def topic(self) -> str | None: ...
    def partition(self) -> int | None: ...
    def offset(self) -> int | None: ...


class KafkaConsumer(Protocol):
    def subscribe(self, topics: list[str]) -> None: ...
    def poll(self, timeout: float) -> KafkaMessage | None: ...
    def commit(self, *, message: KafkaMessage, asynchronous: bool) -> Any: ...
    def seek(self, partition: Any) -> None: ...
    def close(self) -> None: ...


def run_consumer(
    consumer: KafkaConsumer,
    topic: str,
    handlers: Mapping[str, Handler],
    stop: threading.Event,
    *,
    rewind: Callable[[KafkaMessage], Any],
    retry_backoff_s: float = 2.0,
    heartbeat: Callable[[], None] = lambda: None,
) -> None:
    """Consume until `stop` is set. Offsets are committed only after the event's transaction."""
    consumer.subscribe([topic])
    try:
        while not stop.is_set():
            heartbeat()
            message = consumer.poll(1.0)
            if message is None:
                continue
            if message.error():
                log.warning("kafka consumer error", extra={"error": str(message.error())})
                continue
            close_old_connections()
            try:
                event_type, outcome = process_event(message.value(), handlers)
            except Exception:
                # Database down or a bug: don't commit. Rewind to this message and try again later.
                log.exception("event handling failed; will retry")
                connection.close()
                consumer.seek(rewind(message))
                stop.wait(retry_backoff_s)
                continue
            known_type = event_type if event_type in handlers else "other"
            DISPATCH_EVENTS.labels(known_type, outcome).inc()
            consumer.commit(message=message, asynchronous=False)
    finally:
        consumer.close()  # leaves the group cleanly, so partitions move to other replicas at once
