"""`python manage.py purge_expired`: delete what this service promised to keep only for a while.

- idempotency records after IDEMPOTENCY_TTL_HOURS (24 h);
- quotes after 24 h (they hold drop-off PII; orders keep their own copy);
- processed-event IDs after 8 days (Kafka keeps dispatch.events 7 days, so older IDs can't come
  back);
- published outbox rows after 7 days.

Deletes in small batches, so it never holds long locks on hot tables. Run it hourly (a CronJob).
"""

from __future__ import annotations

from datetime import timedelta
from typing import Any

from django.conf import settings
from django.core.management.base import BaseCommand, CommandParser
from django.db.models import Model, QuerySet
from django.utils import timezone

from messaging.models import OutboxEvent, ProcessedEvent
from orders.models import IdempotencyRecord, Quote


def delete_in_batches(queryset: QuerySet[Any], batch_size: int) -> int:
    model: type[Model] = queryset.model
    deleted = 0
    while True:
        batch = list(queryset.values_list("pk", flat=True)[:batch_size])
        if not batch:
            return deleted
        model._default_manager.filter(pk__in=batch).delete()
        deleted += len(batch)


class Command(BaseCommand):
    help = "Delete expired idempotency records, quotes, inbox IDs and published outbox rows."

    def add_arguments(self, parser: CommandParser) -> None:
        parser.add_argument("--batch-size", type=int, default=1000)

    def handle(self, *args: Any, **options: Any) -> None:
        now = timezone.now()
        size = options["batch_size"]
        counts = {
            "idempotency_records": delete_in_batches(
                IdempotencyRecord.objects.filter(
                    created_at__lt=now - timedelta(hours=settings.IDEMPOTENCY_TTL_HOURS)
                ),
                size,
            ),
            "quotes": delete_in_batches(
                Quote.objects.filter(created_at__lt=now - timedelta(hours=24)), size
            ),
            "processed_events": delete_in_batches(
                ProcessedEvent.objects.filter(processed_at__lt=now - timedelta(days=8)), size
            ),
            "outbox_events": delete_in_batches(
                OutboxEvent.objects.filter(published_at__lt=now - timedelta(days=7)), size
            ),
        }
        self.stdout.write(", ".join(f"{name}: {count}" for name, count in counts.items()))
