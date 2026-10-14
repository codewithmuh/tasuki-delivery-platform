"""`python manage.py consume_dispatch_events`: apply dispatch.events to orders.

Replicas share the topic's partitions through the consumer group (KAFKA_CONSUMER_GROUP).
"""

from __future__ import annotations

import logging
import threading
from typing import Any

from django.conf import settings
from django.core.management.base import BaseCommand

from messaging.consumer import run_consumer
from messaging.kafka import build_consumer, rewind_to
from messaging.worker import Heartbeat, serve_worker_endpoints, stop_on_signals
from orders.dispatch_events import HANDLERS

log = logging.getLogger("orders.consumer")


class Command(BaseCommand):
    help = "Consume dispatch.events until SIGTERM."

    def handle(self, *args: Any, **options: Any) -> None:
        stop = threading.Event()
        stop_on_signals(stop)
        heartbeat = Heartbeat()
        server = serve_worker_endpoints(settings.WORKER_METRICS_PORT, heartbeat)
        consumer = build_consumer(settings.KAFKA_BROKERS, settings.KAFKA_CONSUMER_GROUP)
        log.info("dispatch consumer started", extra={"topic": settings.KAFKA_DISPATCH_TOPIC})
        try:
            run_consumer(
                consumer,
                settings.KAFKA_DISPATCH_TOPIC,
                HANDLERS,
                stop,
                rewind=rewind_to,
                heartbeat=heartbeat.beat,
            )
        finally:
            server.shutdown()
            log.info("dispatch consumer stopped")
