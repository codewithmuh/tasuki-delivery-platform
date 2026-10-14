"""What we publish validates against contracts/, and our copy of wards.json matches it."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from django.conf import settings
from django.utils import timezone
from jsonschema import Draft202012Validator

from messaging.relay import relay_batch
from orders.models import Order
from orders.services import on_events_published
from tests.helpers import (
    MEGURO,
    CourierCtx,
    FakePublisher,
    Tenant,
    assign,
    deliver_dispatch_event,
    dispatch_event,
    order,
)

CONTRACTS = Path(__file__).resolve().parents[3] / "contracts"
SERVICE = Path(__file__).resolve().parents[1]


def _validator(name: str) -> Draft202012Validator:
    schema = json.loads((CONTRACTS / "events" / name).read_text())
    Draft202012Validator.check_schema(schema)
    return Draft202012Validator(schema, format_checker=Draft202012Validator.FORMAT_CHECKER)


def _drain() -> list[dict[str, Any]]:
    publisher = FakePublisher()
    while relay_batch(publisher, on_published=on_events_published).published:
        pass
    return publisher.payloads()


def test_every_order_event_type_validates(
    tenant: Tenant, courier: CourierCtx, settings: Any
) -> None:
    validator = _validator("order-event.v1.schema.json")

    cancelled = order(tenant)["order_id"]
    tenant.client.post(f"/v1/orders/{cancelled}/cancel", {"reason": "other"}, format="json")

    delivered = order(tenant)["order_id"]
    assign(delivered, courier.courier.id)
    courier.client.post(f"/v1/courier/orders/{delivered}/pickup")
    courier.client.post(f"/v1/courier/orders/{delivered}/deliver", MEGURO, format="json")

    failed = order(tenant)["order_id"]
    assign(failed, courier.courier.id)
    courier.client.post(f"/v1/courier/orders/{failed}/pickup")
    courier.client.post(f"/v1/courier/orders/{failed}/fail", {}, format="json")

    timed_out = order(tenant)["order_id"]
    Order.objects.filter(pk=timed_out).update(
        created_at=timezone.now() - timezone.timedelta(hours=1)
    )
    deliver_dispatch_event(dispatch_event("NoCourierAvailable", timed_out))

    events = _drain()
    assert {e["type"] for e in events} == {
        "OrderConfirmed",
        "OrderCancelled",
        "OrderPickedUp",
        "OrderDelivered",
        "OrderFailed",
    }
    for event in events:
        errors = sorted(validator.iter_errors(event), key=str)
        assert not errors, f"{event['type']}: {errors}"
    picked_up = next(e for e in events if e["type"] == "OrderPickedUp")
    assert picked_up["order"]["courier_id"] == courier.courier.id
    assert all(e["event_id"].startswith("evt_") for e in events)
    assert len({e["event_id"] for e in events}) == len(events)


def test_test_dispatch_events_match_the_dispatch_contract() -> None:
    # The consumer tests feed events built by tests.helpers.dispatch_event; keep them honest.
    validator = _validator("dispatch-event.v1.schema.json")
    event = dispatch_event(
        "CourierAssigned",
        "ord_01J00000000000000000000000",
        courier_id="cou_01J00000000000000000000000",
    )
    event["event_id"] = "evt_01J00000000000000000000000"
    assert not list(validator.iter_errors(event))


def test_wards_copy_matches_the_contract() -> None:
    contract = json.loads((CONTRACTS / "data" / "wards.json").read_text())
    copy = json.loads((SERVICE / "src" / "core" / "data" / "wards.json").read_text())
    assert copy == contract, "copy contracts/data/wards.json to src/core/data/wards.json"


def test_openapi_yaml_is_up_to_date() -> None:
    from drf_spectacular.generators import SchemaGenerator
    from drf_spectacular.renderers import OpenApiYamlRenderer

    schema = SchemaGenerator().get_schema(request=None, public=True)
    rendered = OpenApiYamlRenderer().render(schema, renderer_context={}).decode()
    committed = (SERVICE / "openapi.yaml").read_text()
    assert rendered == committed, "run `make orders-openapi` and commit openapi.yaml"


@pytest.mark.parametrize("path", ["/v1/quotes", "/v1/orders", "/v1/orders/{order_id}/cancel"])
def test_documented_paths(path: str) -> None:
    assert f"  {path}:" in (SERVICE / "openapi.yaml").read_text()


def test_settings_topics_match_the_contract() -> None:
    assert settings.KAFKA_ORDERS_TOPIC == "orders.events"
    assert settings.KAFKA_DISPATCH_TOPIC == "dispatch.events"
