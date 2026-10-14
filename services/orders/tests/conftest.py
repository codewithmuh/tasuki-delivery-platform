from __future__ import annotations

import os
from collections.abc import Callable, Iterator

import httpx
import pytest
import respx
from django.conf import settings
from django.utils import timezone
from rest_framework.test import APIClient

from accounts.models import ApiKey, Courier, Merchant, Store
from core import lifecycle
from core.tokens import issue_courier_token
from tests.helpers import DISPATCH_ETA, SHIBUYA, CourierCtx, Tenant


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    if settings.DATABASES["default"]["ENGINE"] == "django.db.backends.postgresql":
        return
    if os.environ.get("REQUIRE_POSTGRES"):
        # CI sets this, so a broken DATABASE_URL fails the build instead of skipping the race tests.
        raise pytest.UsageError("REQUIRE_POSTGRES is set but DATABASE_URL is not PostgreSQL")
    skip = pytest.mark.skip(reason="needs PostgreSQL: set DATABASE_URL=postgres://...")
    for item in items:
        if "postgres" in item.keywords:
            item.add_marker(skip)


@pytest.fixture(autouse=True)
def _reset_lifecycle() -> Iterator[None]:
    yield
    lifecycle.reset()


@pytest.fixture(autouse=True)
def dispatch() -> Iterator[respx.Route]:
    """dispatch's GET /v1/eta, answering quickly unless a test says otherwise."""
    with respx.mock(base_url=settings.DISPATCH_URL, assert_all_called=False) as mock:
        route = mock.get("/v1/eta", name="eta")
        route.return_value = httpx.Response(200, json=DISPATCH_ETA)
        yield route


@pytest.fixture
def make_tenant(db: None) -> Callable[..., Tenant]:
    def make(name: str = "Ramen Shibuya", prep_minutes: int = 10) -> Tenant:
        merchant = Merchant.objects.create(name=name)
        _, api_key = ApiKey.issue(merchant)
        store = Store.objects.create(
            merchant=merchant, name=f"{name} store", prep_minutes=prep_minutes, **SHIBUYA
        )
        client = APIClient()
        client.credentials(HTTP_AUTHORIZATION=f"Bearer {api_key}")
        return Tenant(merchant, api_key, store, client)

    return make


@pytest.fixture
def tenant(make_tenant: Callable[..., Tenant]) -> Tenant:
    return make_tenant()


@pytest.fixture
def other_tenant(make_tenant: Callable[..., Tenant]) -> Tenant:
    return make_tenant("Sushi Minato")


@pytest.fixture
def make_courier(db: None) -> Callable[..., CourierCtx]:
    def make(name: str = "Kenji") -> CourierCtx:
        courier = Courier.objects.create(name=name, vehicle=Courier.Vehicle.BICYCLE)
        token = issue_courier_token(courier.id, timezone.now())
        client = APIClient()
        client.credentials(HTTP_AUTHORIZATION=f"Bearer {token}")
        return CourierCtx(courier, token, client)

    return make


@pytest.fixture
def courier(make_courier: Callable[..., CourierCtx]) -> CourierCtx:
    return make_courier()


@pytest.fixture
def admin_client() -> APIClient:
    client = APIClient()
    client.credentials(HTTP_AUTHORIZATION=f"Bearer {settings.ADMIN_TOKEN}")
    return client
