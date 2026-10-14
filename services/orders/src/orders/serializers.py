"""Request and response bodies for quotes and orders (contracts/README.md, "Response shapes").

Responses never echo the customer's name, phone or address: the merchant already has them, and the
order-creation response is stored verbatim as the idempotency record, which is not encrypted.
"""

from __future__ import annotations

from typing import Any

from drf_spectacular.utils import extend_schema_field
from rest_framework import serializers

from accounts.serializers import WARD_CHOICES
from core.geo import TOKYO_BBOX
from core.tokens import tracking_url
from orders.models import Order, OrderEvent
from orders.state_machine import OrderStatus

CANCEL_REASONS = ["merchant_request", "customer_request", "out_of_stock", "other"]
FAILURE_REASONS = ["customer_unreachable", "address_not_found", "refused", "other"]


class DropoffSerializer(serializers.Serializer[dict[str, Any]]):
    lat = serializers.FloatField(min_value=TOKYO_BBOX.min_lat, max_value=TOKYO_BBOX.max_lat)
    lng = serializers.FloatField(min_value=TOKYO_BBOX.min_lng, max_value=TOKYO_BBOX.max_lng)
    ward = serializers.ChoiceField(choices=WARD_CHOICES)
    address = serializers.CharField(max_length=500)
    customer_name = serializers.CharField(max_length=200)
    customer_phone = serializers.RegexField(
        r"^[0-9+()\- ]{7,20}$",
        error_messages={"invalid": "Use 7-20 digits, spaces, '+', '-' or parentheses."},
    )


class QuoteRequestSerializer(serializers.Serializer[dict[str, Any]]):
    store_id = serializers.CharField(max_length=30)
    dropoff = DropoffSerializer()


class QuoteSerializer(serializers.Serializer[Any]):
    quote_id = serializers.CharField(source="id")
    fee_yen = serializers.IntegerField(help_text="Delivery fee including 10 % consumption tax.")
    fee_excl_tax_yen = serializers.IntegerField()
    tax_yen = serializers.IntegerField(help_text="10 % of fee_excl_tax_yen, rounded down.")
    distance_m = serializers.IntegerField(help_text="Straight-line distance store to drop-off.")
    eta_seconds = serializers.IntegerField()
    promised_at = serializers.DateTimeField(
        help_text="Indicative. The order's own promise is fixed when it is created."
    )
    expires_at = serializers.DateTimeField()
    eta_source = serializers.ChoiceField(choices=["dispatch", "fallback"])


class OrderCreateSerializer(serializers.Serializer[dict[str, Any]]):
    quote_id = serializers.CharField(max_length=30)
    dropoff = DropoffSerializer(
        required=False,
        help_text="Optional. If sent, it must equal the quoted drop-off (422 dropoff_mismatch).",
    )


class OrderCreatedSerializer(serializers.Serializer[Order]):
    order_id = serializers.CharField(source="id")
    status = serializers.ChoiceField(choices=OrderStatus.choices)
    promised_at = serializers.DateTimeField()
    fee_yen = serializers.IntegerField()
    tracking_url = serializers.SerializerMethodField()
    created_at = serializers.DateTimeField()

    def get_tracking_url(self, order: Order) -> str:
        return tracking_url(order.id, order.promised_at)


class HistorySerializer(serializers.Serializer[OrderEvent]):
    status = serializers.CharField(source="to_status")
    from_status = serializers.CharField(allow_null=True)
    actor = serializers.CharField(help_text="merchant:…, courier:…, dispatch, relay")
    reason = serializers.CharField(allow_null=True)
    occurred_at = serializers.DateTimeField()

    def to_representation(self, instance: OrderEvent) -> dict[str, Any]:
        data: dict[str, Any] = super().to_representation(instance)
        data["from_status"] = data["from_status"] or None
        data["reason"] = data["reason"] or None
        return data


class PointSerializer(serializers.Serializer[Any]):
    lat = serializers.FloatField()
    lng = serializers.FloatField()
    ward = serializers.CharField()


class OrderDetailSerializer(OrderCreatedSerializer):
    store_id = serializers.CharField()
    dropoff = serializers.SerializerMethodField()
    status_reason = serializers.CharField(allow_null=True)
    fee_excl_tax_yen = serializers.IntegerField()
    tax_yen = serializers.IntegerField()
    ready_at = serializers.DateTimeField()
    courier_id = serializers.CharField(allow_null=True)
    picked_up_at = serializers.DateTimeField(allow_null=True)
    delivered_at = serializers.DateTimeField(allow_null=True)
    updated_at = serializers.DateTimeField()
    history = HistorySerializer(many=True)

    @extend_schema_field(PointSerializer)
    def get_dropoff(self, order: Order) -> dict[str, Any]:
        return {"lat": order.dropoff_lat, "lng": order.dropoff_lng, "ward": order.dropoff_ward}

    def to_representation(self, instance: Order) -> dict[str, Any]:
        data: dict[str, Any] = super().to_representation(instance)
        data["status_reason"] = data["status_reason"] or None
        return data


class CancelSerializer(serializers.Serializer[dict[str, Any]]):
    reason = serializers.ChoiceField(choices=CANCEL_REASONS, default="merchant_request")


class DeliverSerializer(serializers.Serializer[dict[str, Any]]):
    lat = serializers.FloatField(min_value=-90, max_value=90)
    lng = serializers.FloatField(min_value=-180, max_value=180)


class FailSerializer(serializers.Serializer[dict[str, Any]]):
    reason = serializers.ChoiceField(choices=FAILURE_REASONS, default="customer_unreachable")


class CourierOrderSerializer(serializers.Serializer[Order]):
    order_id = serializers.CharField(source="id")
    status = serializers.ChoiceField(choices=OrderStatus.choices)
