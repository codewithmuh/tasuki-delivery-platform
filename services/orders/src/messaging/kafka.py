"""confluent-kafka adapters. Kafka-specific code lives only here, so the rest tests without it."""

from __future__ import annotations

import logging
from collections.abc import Callable, Sequence
from typing import Any

from confluent_kafka import Consumer, KafkaError, Producer, TopicPartition

from messaging.consumer import KafkaMessage
from messaging.relay import Message

log = logging.getLogger("orders.kafka")


class KafkaPublisher:
    """Publishes a batch and waits for every acknowledgement, or for the delivery timeout."""

    def __init__(
        self, brokers: str, *, delivery_timeout_s: float = 10.0, producer: Any | None = None
    ) -> None:
        self._delivery_timeout_s = delivery_timeout_s
        self._producer = producer or Producer(
            {
                "bootstrap.servers": brokers,
                "client.id": "orders-outbox-relay",
                "logger": log,  # librdkafka's own messages as JSON log lines, not raw stderr
                # acks=all and no duplicates or reordering caused by the producer's own retries.
                "enable.idempotence": True,
                "acks": "all",
                "linger.ms": 5,
                # librdkafka gives up on a message after this, and reports it as failed. It matches
                # the flush wait below, so a message we didn't mark published isn't still queued
                # here and sent later alongside the copy the next relay round produces.
                "message.timeout.ms": int(delivery_timeout_s * 1000),
            }
        )

    def publish(self, messages: Sequence[Message]) -> set[str]:
        acknowledged: set[str] = set()

        def on_delivery(event_id: str) -> Callable[[KafkaError | None, Any], None]:
            def callback(error: KafkaError | None, _message: Any) -> None:
                if error is None:
                    acknowledged.add(event_id)
                else:
                    log.warning(
                        "kafka delivery failed", extra={"event_id": event_id, "error": str(error)}
                    )

            return callback

        for message in messages:
            self._producer.produce(
                message.topic,
                key=message.key.encode(),
                value=message.value,
                headers=list(message.headers),
                on_delivery=on_delivery(message.event_id),
            )
        # flush() serves the delivery callbacks; the extra second lets the timeouts above fire
        # first.
        undelivered = self._producer.flush(self._delivery_timeout_s + 1)
        if undelivered:
            log.warning("kafka flush timed out", extra={"undelivered": undelivered})
        return acknowledged

    def close(self) -> None:
        self._producer.flush(self._delivery_timeout_s)


def build_consumer(brokers: str, group_id: str) -> Any:
    return Consumer(
        {
            "bootstrap.servers": brokers,
            "group.id": group_id,
            "client.id": "orders-dispatch-consumer",
            "logger": log,
            # Offsets are committed by hand, after the event's database transaction has committed.
            "enable.auto.commit": False,
            # A new consumer group starts from the oldest retained event rather than skipping the
            # backlog.
            "auto.offset.reset": "earliest",
        }
    )


def rewind_to(message: KafkaMessage) -> TopicPartition:
    """The position of `message`, for Consumer.seek(): read it again on the next poll."""
    topic, partition, offset = message.topic(), message.partition(), message.offset()
    if topic is None or partition is None or offset is None:
        raise ValueError("a consumed message always has a topic, partition and offset")
    return TopicPartition(topic, partition, offset)
