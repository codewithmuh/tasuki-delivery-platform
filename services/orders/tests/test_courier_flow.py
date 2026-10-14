"""Courier endpoints: pickup, deliver (with proof), fail; only the assigned courier may act."""

from __future__ import annotations

from collections.abc import Callable

import pytest
from django.test import override_settings

from messaging.models import OutboxEvent
from orders.models import Order
from tests.helpers import MEGURO, CourierCtx, Tenant, assign, order


def _types(order_id: str) -> list[str]:
    return [e.payload["type"] for e in OutboxEvent.objects.filter(key=order_id).order_by("id")]


def test_full_delivery(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)

    picked = courier.client.post(f"/v1/courier/orders/{order_id}/pickup")
    assert picked.status_code == 200
    assert picked.json() == {"order_id": order_id, "status": "picked_up"}

    delivered = courier.client.post(
        f"/v1/courier/orders/{order_id}/deliver",
        {"lat": MEGURO["lat"] + 0.001, "lng": MEGURO["lng"]},  # ~110 m away: fine
        format="json",
    )
    assert delivered.status_code == 200
    assert delivered.json() == {"order_id": order_id, "status": "delivered"}

    row = Order.objects.get(pk=order_id)
    assert row.courier_id == courier.courier.id
    assert row.picked_up_at is not None
    assert row.delivered_at is not None
    assert row.delivery_lat == pytest.approx(MEGURO["lat"] + 0.001)
    assert _types(order_id) == ["OrderConfirmed", "OrderPickedUp", "OrderDelivered"]
    history = tenant.client.get(f"/v1/orders/{order_id}").json()["history"]
    assert [h["status"] for h in history] == [
        "confirmed",
        "dispatching",
        "assigned",
        "picked_up",
        "delivered",
    ]
    picked_up_event = OutboxEvent.objects.filter(key=order_id).order_by("id")[1]
    assert picked_up_event.payload["order"]["courier_id"] == courier.courier.id


def test_pickup_with_empty_body_and_no_content_type(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    response = courier.client.generic("POST", f"/v1/courier/orders/{order_id}/pickup")
    assert response.status_code == 200


def test_only_the_assigned_courier_can_act(
    tenant: Tenant, courier: CourierCtx, make_courier: Callable[..., CourierCtx]
) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    intruder = make_courier("Intruder")
    response = intruder.client.post(f"/v1/courier/orders/{order_id}/pickup")
    assert response.status_code == 404
    assert Order.objects.get(pk=order_id).status == "assigned"


def test_unassigned_order_is_404_for_couriers(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assert courier.client.post(f"/v1/courier/orders/{order_id}/pickup").status_code == 404


def test_deliver_before_pickup_is_409(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    response = courier.client.post(f"/v1/courier/orders/{order_id}/deliver", MEGURO, format="json")
    assert response.status_code == 409
    assert response.json()["error"]["code"] == "illegal_transition"


def test_delivery_proof_must_be_near_the_dropoff(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    courier.client.post(f"/v1/courier/orders/{order_id}/pickup")
    response = courier.client.post(
        f"/v1/courier/orders/{order_id}/deliver",
        {"lat": MEGURO["lat"] + 0.01, "lng": MEGURO["lng"]},  # ~1.1 km away
        format="json",
    )
    assert response.status_code == 422
    assert response.json()["error"]["code"] == "proof_too_far"
    assert Order.objects.get(pk=order_id).status == "picked_up"


def test_proof_radius_is_configurable(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    courier.client.post(f"/v1/courier/orders/{order_id}/pickup")
    with override_settings(DELIVERY_PROOF_RADIUS_M=2_000):
        response = courier.client.post(
            f"/v1/courier/orders/{order_id}/deliver",
            {"lat": MEGURO["lat"] + 0.01, "lng": MEGURO["lng"]},
            format="json",
        )
    assert response.status_code == 200


def test_deliver_needs_coordinates(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    response = courier.client.post(f"/v1/courier/orders/{order_id}/deliver", {}, format="json")
    assert response.status_code == 400


def test_failed_delivery(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    courier.client.post(f"/v1/courier/orders/{order_id}/pickup")
    response = courier.client.post(
        f"/v1/courier/orders/{order_id}/fail", {"reason": "customer_unreachable"}, format="json"
    )
    assert response.status_code == 200
    assert response.json()["status"] == "failed"
    failed = OutboxEvent.objects.filter(key=order_id).order_by("id").last()
    assert failed is not None
    assert failed.payload["type"] == "OrderFailed"
    assert failed.payload["reason"] == "customer_unreachable"


def test_cannot_fail_before_pickup(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    response = courier.client.post(f"/v1/courier/orders/{order_id}/fail", {}, format="json")
    assert response.status_code == 409


def test_cancelled_after_assignment_is_409_for_the_courier(
    tenant: Tenant, courier: CourierCtx
) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    assert tenant.client.post(f"/v1/orders/{order_id}/cancel", {}, format="json").status_code == 200
    response = courier.client.post(f"/v1/courier/orders/{order_id}/pickup")
    assert response.status_code == 409


def test_merchant_cannot_cancel_after_pickup(tenant: Tenant, courier: CourierCtx) -> None:
    order_id = order(tenant)["order_id"]
    assign(order_id, courier.courier.id)
    courier.client.post(f"/v1/courier/orders/{order_id}/pickup")
    response = tenant.client.post(f"/v1/orders/{order_id}/cancel", {}, format="json")
    assert response.status_code == 409
    assert (
        response.json()["error"]["message"] == "An order that is picked_up cannot become cancelled."
    )
