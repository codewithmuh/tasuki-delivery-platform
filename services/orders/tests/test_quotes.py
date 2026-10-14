"""POST /v1/quotes: price, ETA from dispatch, and the fallback when dispatch is slow or down."""

from __future__ import annotations

from datetime import datetime, timedelta
from typing import Any

import httpx
import pytest
import respx
from django.db import connection
from prometheus_client import REGISTRY

from orders.eta import fallback_eta_seconds
from orders.models import Quote
from tests.helpers import PII, Tenant, dropoff, quote


def _fallbacks(reason: str) -> float:
    return REGISTRY.get_sample_value("orders_eta_fallbacks_total", {"reason": reason}) or 0.0


def test_quote_response_shape_and_price(tenant: Tenant, dispatch: respx.Route) -> None:
    body = quote(tenant)
    assert set(body) >= {
        "quote_id",
        "fee_yen",
        "fee_excl_tax_yen",
        "tax_yen",
        "eta_seconds",
        "promised_at",
        "expires_at",
        "eta_source",
    }
    assert body["quote_id"].startswith("quo_")
    # Shibuya to Meguro is about 2.5 km: 6 started steps of 500 m.
    assert (body["fee_excl_tax_yen"], body["tax_yen"], body["fee_yen"]) == (750, 75, 825)
    assert body["eta_source"] == "dispatch"
    assert body["eta_seconds"] == 1800
    expires = datetime.fromisoformat(body["expires_at"])
    promised = datetime.fromisoformat(body["promised_at"])
    assert expires - promised + timedelta(seconds=1800) == timedelta(seconds=120)
    request = dispatch.calls.last.request
    assert request.url.params["ward"] == "shibuya"
    assert float(request.url.params["dropoff_lat"]) == pytest.approx(35.6415)


def test_dispatch_eta_never_beats_the_kitchen(tenant: Tenant, dispatch: respx.Route) -> None:
    # Dispatch says 5 minutes, but the store needs 10 minutes and the ride takes 13.
    dispatch.return_value = httpx.Response(
        200, json={"eta_seconds": 300, "pickup_s": 60, "travel_s": 780, "supply": "ok"}
    )
    assert quote(tenant)["eta_seconds"] == 10 * 60 + 780


@pytest.mark.parametrize(
    ("side_effect", "reason"),
    [
        (httpx.ReadTimeout("slow"), "timeout"),
        (httpx.ConnectTimeout("slow"), "timeout"),
        (httpx.ConnectError("refused"), "unavailable"),
        (httpx.Response(503), "unavailable"),
        (httpx.Response(200, text="<html>"), "bad_response"),
        (httpx.Response(200, json=["not", "an", "object"]), "bad_response"),
        (httpx.Response(200, json={"eta_seconds": "soon"}), "bad_response"),
        (httpx.Response(200, json={"eta_seconds": -5}), "bad_response"),
        (httpx.Response(200, json={"eta_seconds": True}), "bad_response"),
        (httpx.Response(200, json={"eta_seconds": 99_999_999}), "bad_response"),
        (httpx.Response(200, json={"supply": "ok"}), "bad_response"),
    ],
)
def test_eta_falls_back_when_dispatch_fails(
    tenant: Tenant, dispatch: respx.Route, side_effect: Any, reason: str
) -> None:
    dispatch.side_effect = side_effect if isinstance(side_effect, Exception) else None
    if isinstance(side_effect, httpx.Response):
        dispatch.return_value = side_effect
    before = _fallbacks(reason)
    body = quote(tenant)
    assert body["eta_source"] == "fallback"
    distance = Quote.objects.get(pk=body["quote_id"]).distance_m
    assert body["eta_seconds"] == fallback_eta_seconds(distance, tenant.store.prep_minutes)
    assert _fallbacks(reason) == before + 1


def test_fallback_formula() -> None:
    # 2,500 m x 1.35 detour at 15 km/h = 810 s, plus 10 minutes of prep.
    assert fallback_eta_seconds(2_500, 10) == 600 + 810


def test_dispatch_timeout_is_tight(settings: Any) -> None:
    assert settings.DISPATCH_TIMEOUT_MS <= 300


def test_quote_for_another_merchants_store_is_404(tenant: Tenant, other_tenant: Tenant) -> None:
    response = tenant.client.post(
        "/v1/quotes", {"store_id": other_tenant.store.id, "dropoff": dropoff()}, format="json"
    )
    assert response.status_code == 404
    assert response.json()["error"]["code"] == "store_not_found"


@pytest.mark.parametrize(
    "overrides",
    [
        {"ward": "yokohama"},
        {"lat": 34.69, "lng": 135.50},  # Osaka
        {"customer_phone": "call me"},
        {"customer_phone": "1"},
        {"address": ""},
        {"address": "x" * 501},
    ],
)
def test_invalid_dropoff_is_400(tenant: Tenant, overrides: dict[str, Any]) -> None:
    response = tenant.client.post(
        "/v1/quotes", {"store_id": tenant.store.id, "dropoff": dropoff(**overrides)}, format="json"
    )
    assert response.status_code == 400
    error = response.json()["error"]
    assert error["code"] == "validation_error"
    assert "dropoff" in error["details"]


def test_simulator_style_dropoff_is_accepted(tenant: Tenant) -> None:
    body = quote(
        tenant,
        address="Shibuya-ku 1-2-3 (simulated)",
        customer_name="Sim Customer 000123",
        customer_phone="090-0000-0123",
    )
    assert body["quote_id"]


def test_quote_pii_is_encrypted_at_rest(tenant: Tenant) -> None:
    body = quote(tenant)
    with connection.cursor() as cursor:
        cursor.execute(
            "SELECT dropoff_address, customer_name, customer_phone FROM orders_quote WHERE id = %s",
            [body["quote_id"]],
        )
        raw = cursor.fetchone()
    assert raw is not None
    for stored, plain in zip(raw, PII.values(), strict=True):
        assert plain not in stored
        assert stored.startswith("gAAAA")  # a Fernet token
    loaded = Quote.objects.get(pk=body["quote_id"])
    assert (loaded.dropoff_address, loaded.customer_name, loaded.customer_phone) == tuple(
        PII.values()
    )
