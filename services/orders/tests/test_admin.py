"""Seeding API (contracts/README.md: response shapes)."""

from __future__ import annotations

import jwt
import pytest
from django.conf import settings
from rest_framework.test import APIClient

from accounts.models import ApiKey, Courier, Merchant, Store

pytestmark = pytest.mark.django_db


def test_create_merchant_with_stores(admin_client: APIClient) -> None:
    response = admin_client.post(
        "/v1/admin/merchants",
        {
            "name": "Ramen Ichiban",
            "stores": [
                {"name": "Shibuya", "lat": 35.664, "lng": 139.6982, "ward": "shibuya"},
                {
                    "name": "Minato",
                    "lat": 35.6581,
                    "lng": 139.7516,
                    "ward": "minato",
                    "prep_minutes": 15,
                },
            ],
        },
        format="json",
    )
    assert response.status_code == 201
    body = response.json()
    assert set(body) == {"merchant_id", "api_key", "stores"}
    assert body["merchant_id"].startswith("mer_")
    assert len(body["merchant_id"]) == 30
    assert body["api_key"].startswith("tsk_")
    assert all(set(s) >= {"store_id", "name", "lat", "lng", "ward"} for s in body["stores"])
    assert [store["ward"] for store in body["stores"]] == ["shibuya", "minato"]
    assert Store.objects.get(pk=body["stores"][1]["store_id"]).prep_minutes == 15
    # The key works straight away.
    merchant_client = APIClient()
    merchant_client.credentials(HTTP_AUTHORIZATION=f"Bearer {body['api_key']}")
    assert merchant_client.get("/v1/orders/ord_missing").status_code == 404
    assert ApiKey.objects.filter(merchant_id=body["merchant_id"]).count() == 1


@pytest.mark.parametrize(
    "store",
    [
        {"name": "x", "lat": 35.66, "lng": 139.69, "ward": "yokohama"},  # not a ward
        {"name": "x", "lat": 34.69, "lng": 135.50, "ward": "chuo"},  # Osaka
    ],
)
def test_store_must_be_in_tokyo(admin_client: APIClient, store: dict[str, object]) -> None:
    response = admin_client.post(
        "/v1/admin/merchants", {"name": "x", "stores": [store]}, format="json"
    )
    assert response.status_code == 400
    assert response.json()["error"]["code"] == "validation_error"
    assert not Merchant.objects.exists()  # nothing half-created


def test_create_courier_returns_a_token(admin_client: APIClient) -> None:
    response = admin_client.post(
        "/v1/admin/couriers", {"name": "Kenji", "vehicle": "scooter"}, format="json"
    )
    assert response.status_code == 201
    body = response.json()
    assert set(body) == {"courier_id", "token"}
    claims = jwt.decode(body["token"], settings.COURIER_TOKEN_SECRET, algorithms=["HS256"])
    assert claims["sub"] == body["courier_id"]
    assert Courier.objects.get(pk=body["courier_id"]).vehicle == "scooter"


def test_create_courier_validates_vehicle(admin_client: APIClient) -> None:
    response = admin_client.post(
        "/v1/admin/couriers", {"name": "Kenji", "vehicle": "truck"}, format="json"
    )
    assert response.status_code == 400


def test_reissue_courier_token(admin_client: APIClient) -> None:
    courier = Courier.objects.create(name="Aiko", vehicle="bicycle")
    response = admin_client.post(f"/v1/admin/couriers/{courier.id}/token")
    assert response.status_code == 200
    assert response.json()["courier_id"] == courier.id
    missing = admin_client.post("/v1/admin/couriers/cou_missing/token")
    assert missing.status_code == 404
    assert missing.json()["error"]["code"] == "not_found"
