"""POST /v1/orders, GET /v1/orders/{id}, POST /v1/orders/{id}/cancel."""

from __future__ import annotations

from datetime import datetime, timedelta
from typing import Any

import jwt
import pytest
from django.conf import settings
from django.utils import timezone
from prometheus_client import REGISTRY

from messaging.models import OutboxEvent
from orders.models import IdempotencyRecord, Order, OrderEvent, Quote
from tests.helpers import Tenant, dropoff, order, post_order, quote


def _replays() -> float:
    return REGISTRY.get_sample_value("orders_idempotent_replays_total") or 0.0


def test_create_order(tenant: Tenant) -> None:
    q = quote(tenant)
    response = post_order(tenant, {"quote_id": q["quote_id"]})
    assert response.status_code == 201
    body = response.json()
    assert set(body) == {
        "order_id",
        "status",
        "promised_at",
        "fee_yen",
        "tracking_url",
        "created_at",
    }
    assert body["status"] == "confirmed"
    assert body["fee_yen"] == q["fee_yen"]
    created = datetime.fromisoformat(body["created_at"])
    assert datetime.fromisoformat(body["promised_at"]) - created == timedelta(seconds=1800)

    order_row = Order.objects.get(pk=body["order_id"])
    assert order_row.merchant == tenant.merchant
    assert order_row.ready_at - order_row.created_at == timedelta(minutes=10)
    assert [e.to_status for e in OrderEvent.objects.filter(order=order_row)] == ["confirmed"]
    event = OutboxEvent.objects.get(key=body["order_id"])
    assert event.topic == "orders.events"
    assert event.payload["type"] == "OrderConfirmed"
    assert event.published_at is None


def test_tracking_url_is_a_signed_link_for_this_order(tenant: Tenant) -> None:
    body = order(tenant)
    base, token = body["tracking_url"].rsplit("/", 1)
    assert base == f"{settings.TRACKING_PAGE_URL}/track"
    claims = jwt.decode(token, settings.TRACKING_TOKEN_SECRET, algorithms=["HS256"])
    assert claims["sub"] == body["order_id"]
    assert claims["role"] == "tracking"
    promised = datetime.fromisoformat(body["promised_at"])
    assert claims["exp"] == int((promised + timedelta(hours=24)).timestamp())


# ---- idempotency ---------------------------------------------------------------------------------


def test_retry_with_same_key_replays_the_original_response(tenant: Tenant) -> None:
    q = quote(tenant)
    first = post_order(tenant, {"quote_id": q["quote_id"]}, key="key-1")
    before = _replays()
    second = post_order(tenant, {"quote_id": q["quote_id"]}, key="key-1")
    assert second.status_code == first.status_code == 201
    assert second.json() == first.json()
    assert second.headers["Idempotent-Replayed"] == "true"
    assert "Idempotent-Replayed" not in first.headers
    assert Order.objects.count() == 1
    assert OutboxEvent.objects.count() == 1
    assert _replays() == before + 1


def test_same_key_with_a_different_body_is_422(tenant: Tenant) -> None:
    first, second = quote(tenant), quote(tenant)
    assert post_order(tenant, {"quote_id": first["quote_id"]}, key="key-1").status_code == 201
    response = post_order(tenant, {"quote_id": second["quote_id"]}, key="key-1")
    assert response.status_code == 422
    assert response.json()["error"]["code"] == "idempotency_key_reused"
    assert Order.objects.count() == 1


def test_keys_are_scoped_per_merchant(tenant: Tenant, other_tenant: Tenant) -> None:
    assert post_order(tenant, {"quote_id": quote(tenant)["quote_id"]}, key="k").status_code == 201
    response = post_order(other_tenant, {"quote_id": quote(other_tenant)["quote_id"]}, key="k")
    assert response.status_code == 201
    assert Order.objects.count() == 2


def test_body_key_order_and_whitespace_dont_matter(tenant: Tenant) -> None:
    q = quote(tenant)
    raw_a = f'{{"quote_id": "{q["quote_id"]}"}}'
    raw_b = f'{{ "quote_id" :"{q["quote_id"]}" }}'
    for raw in (raw_a, raw_b):
        response = tenant.client.post(
            "/v1/orders", raw, content_type="application/json", HTTP_IDEMPOTENCY_KEY="same"
        )
        assert response.status_code == 201
    assert Order.objects.count() == 1


@pytest.mark.parametrize("key", [None, "", "has space", "x" * 256, "tab\tkey"])
def test_idempotency_key_is_required_and_sane(tenant: Tenant, key: str | None) -> None:
    headers = {} if key is None else {"HTTP_IDEMPOTENCY_KEY": key}
    response = tenant.client.post(
        "/v1/orders", {"quote_id": quote(tenant)["quote_id"]}, format="json", **headers
    )
    assert response.status_code == 400
    assert response.json()["error"]["code"] == "idempotency_key_required"
    assert not Order.objects.exists()


def test_failed_attempts_are_not_stored(tenant: Tenant) -> None:
    response = post_order(tenant, {"quote_id": "quo_missing"}, key="retry-me")
    assert response.status_code == 404
    assert not IdempotencyRecord.objects.exists()


def test_records_store_the_response_without_personal_data(tenant: Tenant) -> None:
    body = order(tenant)
    record = IdempotencyRecord.objects.get()
    assert record.response_status == 201
    assert record.response_body == body


# ---- quote rules ---------------------------------------------------------------------------------


def test_expired_quote_is_409(tenant: Tenant) -> None:
    q = quote(tenant)
    Quote.objects.filter(pk=q["quote_id"]).update(expires_at=timezone.now() - timedelta(seconds=1))
    response = post_order(tenant, {"quote_id": q["quote_id"]})
    assert response.status_code == 409
    assert response.json()["error"]["code"] == "quote_expired"
    assert not Order.objects.exists()


def test_quote_expires_after_two_minutes(tenant: Tenant) -> None:
    q = Quote.objects.get(pk=quote(tenant)["quote_id"])
    assert q.expires_at - q.created_at == timedelta(minutes=2)
    assert not q.is_expired(q.created_at + timedelta(seconds=119))
    assert q.is_expired(q.created_at + timedelta(seconds=120))


def test_a_quote_buys_one_order(tenant: Tenant) -> None:
    q = quote(tenant)
    assert post_order(tenant, {"quote_id": q["quote_id"]}, key="a").status_code == 201
    response = post_order(tenant, {"quote_id": q["quote_id"]}, key="b")
    assert response.status_code == 409
    assert response.json()["error"]["code"] == "quote_already_used"


def test_unknown_quote_is_404(tenant: Tenant) -> None:
    response = post_order(tenant, {"quote_id": "quo_01J00000000000000000000000"})
    assert response.status_code == 404
    assert response.json()["error"]["code"] == "quote_not_found"


def test_dropoff_if_sent_must_match_the_quote(tenant: Tenant) -> None:
    q = quote(tenant)
    mismatch = post_order(
        tenant, {"quote_id": q["quote_id"], "dropoff": dropoff(address="Somewhere else 9-9")}
    )
    assert mismatch.status_code == 422
    assert mismatch.json()["error"]["code"] == "dropoff_mismatch"
    match = post_order(tenant, {"quote_id": q["quote_id"], "dropoff": dropoff()})
    assert match.status_code == 201


def test_invalid_order_body_is_400(tenant: Tenant) -> None:
    response = post_order(tenant, {"quote": "x"})
    assert response.status_code == 400
    assert response.json()["error"]["details"]["quote_id"]


def test_malformed_json_is_400(tenant: Tenant) -> None:
    response = tenant.client.post(
        "/v1/orders", "{not json", content_type="application/json", HTTP_IDEMPOTENCY_KEY="k"
    )
    assert response.status_code == 400
    assert response.json()["error"]["code"] == "parse_error"


# ---- tenant isolation ---------------------------------------------------------------------------


def test_merchants_cannot_see_or_touch_each_others_orders(
    tenant: Tenant, other_tenant: Tenant
) -> None:
    mine = order(tenant)["order_id"]
    for response in (
        other_tenant.client.get(f"/v1/orders/{mine}"),
        other_tenant.client.post(f"/v1/orders/{mine}/cancel", {}, format="json"),
    ):
        assert response.status_code == 404
        assert response.json()["error"]["code"] == "not_found"
    assert Order.objects.get(pk=mine).status == "confirmed"


def test_merchants_cannot_order_from_each_others_quotes(
    tenant: Tenant, other_tenant: Tenant
) -> None:
    theirs = quote(other_tenant)["quote_id"]
    assert post_order(tenant, {"quote_id": theirs}).status_code == 404


# ---- read and cancel ----------------------------------------------------------------------------


def test_get_order_with_history(tenant: Tenant) -> None:
    created = order(tenant)
    response = tenant.client.get(f"/v1/orders/{created['order_id']}")
    assert response.status_code == 200
    body: dict[str, Any] = response.json()
    assert body["order_id"] == created["order_id"]
    assert body["tracking_url"] == created["tracking_url"]
    assert body["dropoff"] == {"lat": 35.6415, "lng": 139.6982, "ward": "meguro"}
    assert body["courier_id"] is None
    assert body["status_reason"] is None
    assert body["history"] == [
        {
            "status": "confirmed",
            "from_status": None,
            "actor": f"merchant:{tenant.merchant.id}",
            "reason": None,
            "occurred_at": body["history"][0]["occurred_at"],
        }
    ]


def test_cancel_before_pickup(tenant: Tenant) -> None:
    created = order(tenant)
    response = tenant.client.post(
        f"/v1/orders/{created['order_id']}/cancel", {"reason": "out_of_stock"}, format="json"
    )
    assert response.status_code == 200
    body = response.json()
    assert body["status"] == "cancelled"
    assert body["status_reason"] == "out_of_stock"
    assert [h["status"] for h in body["history"]] == ["confirmed", "cancelled"]
    cancelled = OutboxEvent.objects.filter(key=created["order_id"]).order_by("id").last()
    assert cancelled is not None
    assert cancelled.payload["type"] == "OrderCancelled"
    assert cancelled.payload["reason"] == "out_of_stock"


def test_cancel_twice_is_409(tenant: Tenant) -> None:
    created = order(tenant)
    url = f"/v1/orders/{created['order_id']}/cancel"
    assert tenant.client.post(url, {}, format="json").status_code == 200
    again = tenant.client.post(url, {}, format="json")
    assert again.status_code == 409
    assert again.json()["error"]["code"] == "illegal_transition"


def test_cancel_reason_must_be_a_known_code(tenant: Tenant) -> None:
    # Free text could carry personal data into events; reasons are codes.
    created = order(tenant)
    response = tenant.client.post(
        f"/v1/orders/{created['order_id']}/cancel",
        {"reason": "customer Hanako called"},
        format="json",
    )
    assert response.status_code == 400


def test_history_is_append_only(tenant: Tenant) -> None:
    event = OrderEvent.objects.get(order_id=order(tenant)["order_id"])
    event.reason = "rewritten"
    with pytest.raises(RuntimeError, match="append-only"):
        event.save()
