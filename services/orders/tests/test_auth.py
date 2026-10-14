"""Authentication failures for each of the three schemes."""

from __future__ import annotations

from collections.abc import Callable
from datetime import timedelta

import jwt
import pytest
from django.conf import settings
from django.test import override_settings
from django.utils import timezone
from rest_framework.test import APIClient

from accounts.models import ApiKey
from core.tokens import tracking_token
from tests.helpers import CourierCtx, Tenant, order


def _with(token: str) -> APIClient:
    client = APIClient()
    client.credentials(HTTP_AUTHORIZATION=f"Bearer {token}")
    return client


# ---- merchants ----------------------------------------------------------------------------------


@pytest.mark.django_db
def test_no_credentials_is_401_with_a_challenge() -> None:
    response = APIClient().post("/v1/quotes", {}, format="json")
    assert response.status_code == 401
    assert response.json()["error"]["code"] == "not_authenticated"
    assert response.headers["WWW-Authenticate"] == 'Bearer realm="merchant"'


@pytest.mark.django_db
@pytest.mark.parametrize(
    "header",
    ["Basic dXNlcjpwYXNz", "Bearer", "Bearer   ", "tsk_abc_def"],
)
def test_malformed_authorization_is_401(header: str) -> None:
    client = APIClient()
    client.credentials(HTTP_AUTHORIZATION=header)
    assert client.post("/v1/quotes", {}, format="json").status_code == 401


@pytest.mark.parametrize(
    "token", ["tsk_nope_nope", "not-even-close", "tsk_only-two", "xyz_abc_def"]
)
def test_unknown_api_key_is_401(tenant: Tenant, token: str) -> None:
    response = _with(token).post("/v1/quotes", {}, format="json")
    assert response.status_code == 401
    assert response.json()["error"]["code"] == "invalid_api_key"


def test_right_lookup_wrong_secret_is_401(tenant: Tenant) -> None:
    prefix, lookup, _ = tenant.api_key.split("_", 2)
    forged = f"{prefix}_{lookup}_{'A' * 43}"
    assert _with(forged).post("/v1/quotes", {}, format="json").status_code == 401


def test_revoked_key_is_401(tenant: Tenant) -> None:
    ApiKey.objects.filter(merchant=tenant.merchant).update(revoked_at=timezone.now())
    assert tenant.client.post("/v1/quotes", {}, format="json").status_code == 401


def test_inactive_merchant_is_401(tenant: Tenant) -> None:
    tenant.merchant.is_active = False
    tenant.merchant.save()
    assert tenant.client.post("/v1/quotes", {}, format="json").status_code == 401


def test_api_key_is_stored_hashed(tenant: Tenant) -> None:
    key = ApiKey.objects.get(merchant=tenant.merchant)
    assert tenant.api_key not in (key.key_hash, key.lookup)
    assert tenant.api_key.startswith(f"tsk_{key.lookup}_")
    assert len(key.key_hash) == 64


def test_merchant_key_does_not_open_courier_endpoints(tenant: Tenant) -> None:
    created = order(tenant)
    response = tenant.client.post(f"/v1/courier/orders/{created['order_id']}/pickup")
    assert response.status_code == 401


# ---- couriers -----------------------------------------------------------------------------------


def _courier_token(courier_id: str, **overrides: object) -> str:
    now = timezone.now()
    claims = {"sub": courier_id, "role": "courier", "exp": now + timedelta(hours=1), **overrides}
    return jwt.encode(claims, settings.COURIER_TOKEN_SECRET, algorithm="HS256")


@pytest.mark.parametrize(
    "make_token",
    [
        pytest.param(
            lambda cid: _courier_token(cid, exp=timezone.now() - timedelta(minutes=5)),
            id="expired",
        ),
        pytest.param(lambda cid: _courier_token(cid, role="tracking"), id="wrong-role"),
        pytest.param(
            lambda cid: jwt.encode(
                {"sub": cid, "role": "courier", "exp": timezone.now() + timedelta(hours=1)},
                "some-other-secret-that-is-long-enough!!",
                algorithm="HS256",
            ),
            id="wrong-secret",
        ),
        pytest.param(
            lambda cid: jwt.encode(
                {"sub": cid, "role": "courier", "exp": timezone.now() + timedelta(hours=1)},
                key=None,
                algorithm="none",
            ),
            id="alg-none",
        ),
        pytest.param(
            lambda cid: jwt.encode(
                {"sub": cid, "role": "courier"}, settings.COURIER_TOKEN_SECRET, algorithm="HS256"
            ),
            id="no-exp",
        ),
        pytest.param(lambda cid: _courier_token("cou_01JUNKNOWN0000000000000000"), id="unknown"),
        pytest.param(lambda cid: "not.a.jwt", id="garbage"),
    ],
)
def test_bad_courier_tokens_are_401(
    courier: CourierCtx, tenant: Tenant, make_token: Callable[[str], str]
) -> None:
    response = _with(make_token(courier.courier.id)).post("/v1/courier/orders/ord_x/pickup")
    assert response.status_code == 401
    assert response.json()["error"]["code"] in {"invalid_token", "not_authenticated"}
    assert response.headers["WWW-Authenticate"] == 'Bearer realm="courier"'


def test_tracking_link_token_is_not_a_courier_token(courier: CourierCtx) -> None:
    token = tracking_token("ord_01J00000000000000000000000", timezone.now())
    assert _with(token).post("/v1/courier/orders/ord_x/pickup").status_code == 401


def test_inactive_courier_is_401(courier: CourierCtx) -> None:
    courier.courier.is_active = False
    courier.courier.save()
    assert courier.client.post("/v1/courier/orders/ord_x/pickup").status_code == 401


def test_courier_token_claims(courier: CourierCtx) -> None:
    claims = jwt.decode(courier.token, settings.COURIER_TOKEN_SECRET, algorithms=["HS256"])
    assert claims["sub"] == courier.courier.id
    assert claims["role"] == "courier"
    assert claims["exp"] - claims["iat"] == settings.COURIER_TOKEN_TTL_SECONDS
    assert settings.COURIER_TOKEN_TTL_SECONDS >= 24 * 3600  # a simulator run lasts hours


# ---- admin --------------------------------------------------------------------------------------


@pytest.mark.django_db
@pytest.mark.parametrize("token", ["wrong", "test-only-admin-token-but-longer"])
def test_wrong_admin_token_is_401(token: str) -> None:
    response = _with(token).post("/v1/admin/couriers", {"name": "a", "vehicle": "bicycle"})
    assert response.status_code == 401
    assert response.json()["error"]["code"] == "invalid_admin_token"


@pytest.mark.django_db
def test_admin_api_is_off_without_admin_token(admin_client: APIClient) -> None:
    with override_settings(ADMIN_TOKEN=""):
        response = admin_client.post(
            "/v1/admin/couriers", {"name": "a", "vehicle": "bicycle"}, format="json"
        )
    assert response.status_code == 401


def test_merchant_key_does_not_open_admin_endpoints(tenant: Tenant) -> None:
    response = tenant.client.post("/v1/admin/couriers", {"name": "a", "vehicle": "bicycle"})
    assert response.status_code == 401
