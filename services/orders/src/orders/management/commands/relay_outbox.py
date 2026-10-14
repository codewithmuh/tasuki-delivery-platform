"""`python manage.py relay_outbox`: publish the transactional outbox to Kafka (orders.events).

Run one or more replicas; see messaging.relay for why that is safe.
"""

from __future__ import annotations

import logging
import threading
from typing import Any

from django.conf import settings
from django.core.management.base import BaseCommand, CommandParser

from messaging.kafka import KafkaPublisher
from messaging.relay import relay_batch, run_relay
from messaging.worker import Heartbeat, serve_worker_endpoints, stop_on_signals
from orders.services import on_events_published

log = logging.getLogger("orders.relay")


class Command(BaseCommand):
    help = "Publish pending outbox events to Kafka until SIGTERM."

    def add_arguments(self, parser: CommandParser) -> None:
        parser.add_argument("--batch-size", type=int, default=100)
        parser.add_argument("--poll-interval", type=float, default=0.5, help="seconds")
        parser.add_argument("--once", action="store_true", help="publish one batch and exit")

    def handle(self, *args: Any, **options: Any) -> None:
        publisher = KafkaPublisher(settings.KAFKA_BROKERS)
        if options["once"]:
            result = relay_batch(
                publisher, batch_size=options["batch_size"], on_published=on_events_published
            )
            publisher.close()
            self.stdout.write(f"published {result.published}, failed {result.failed}")
            return

        stop = threading.Event()
        stop_on_signals(stop)
        heartbeat = Heartbeat()
        server = serve_worker_endpoints(settings.WORKER_METRICS_PORT, heartbeat)
        log.info("outbox relay started", extra={"topic": settings.KAFKA_ORDERS_TOPIC})
        try:
            run_relay(
                publisher,
                stop,
                batch_size=options["batch_size"],
                poll_interval_s=options["poll_interval"],
                on_published=on_events_published,
                heartbeat=heartbeat.beat,
            )
        finally:
            publisher.close()
            server.shutdown()
            log.info("outbox relay stopped")
