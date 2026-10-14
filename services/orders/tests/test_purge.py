from __future__ import annotations

import io
from datetime import timedelta

from django.core.management import call_command
from django.utils import timezone

from messaging.models import OutboxEvent, ProcessedEvent
from messaging.relay import relay_batch
from orders.models import IdempotencyRecord, Order, Quote
from tests.helpers import FakePublisher, Tenant, order, quote


def test_purge_expired(tenant: Tenant) -> None:
    old = timezone.now() - timedelta(days=9)
    kept_order = order(tenant)
    unused_quote = quote(tenant)
    relay_batch(FakePublisher())
    ProcessedEvent.objects.create(event_id="evt_old", processed_at=old)
    ProcessedEvent.objects.create(event_id="evt_new")
    # Age everything except the fresh inbox entry.
    IdempotencyRecord.objects.update(created_at=old)
    Quote.objects.update(created_at=old)
    OutboxEvent.objects.update(published_at=old)

    out = io.StringIO()
    call_command("purge_expired", "--batch-size", "1", stdout=out)

    assert out.getvalue().strip() == (
        "idempotency_records: 1, quotes: 2, processed_events: 1, outbox_events: 1"
    )
    assert not Quote.objects.filter(pk=unused_quote["quote_id"]).exists()
    # Orders outlive their quotes; the link is cleared.
    kept = Order.objects.get(pk=kept_order["order_id"])
    assert kept.quote_id is None
    assert kept.customer_name
    assert list(ProcessedEvent.objects.values_list("event_id", flat=True)) == ["evt_new"]


def test_purge_keeps_recent_and_unpublished_rows(tenant: Tenant) -> None:
    order(tenant)  # its outbox event is still pending
    call_command("purge_expired", stdout=io.StringIO())
    assert IdempotencyRecord.objects.count() == 1
    assert Quote.objects.count() == 1
    assert OutboxEvent.objects.count() == 1
