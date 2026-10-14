"""Admin/seeding request and response bodies (contracts/README.md, "Response shapes")."""

from __future__ import annotations

from typing import Any

from rest_framework import serializers

from accounts.models import Courier
from core.geo import TOKYO_BBOX, WARD_IDS

WARD_CHOICES = sorted(WARD_IDS)


class StoreInputSerializer(serializers.Serializer[dict[str, Any]]):
    name = serializers.CharField(max_length=200)
    lat = serializers.FloatField()
    lng = serializers.FloatField()
    ward = serializers.ChoiceField(choices=WARD_CHOICES)
    prep_minutes = serializers.IntegerField(min_value=0, max_value=180, default=10)

    def validate(self, attrs: dict[str, Any]) -> dict[str, Any]:
        if not TOKYO_BBOX.contains(attrs["lat"], attrs["lng"]):
            raise serializers.ValidationError("The store must be inside Tokyo's 23 wards.")
        return attrs


class MerchantCreateSerializer(serializers.Serializer[dict[str, Any]]):
    name = serializers.CharField(max_length=200)
    stores: serializers.ListSerializer[Any] = serializers.ListSerializer(
        child=StoreInputSerializer(), max_length=100, default=list
    )


class StoreSerializer(serializers.Serializer[Any]):
    store_id = serializers.CharField(source="id")
    name = serializers.CharField()
    lat = serializers.FloatField()
    lng = serializers.FloatField()
    ward = serializers.CharField()
    prep_minutes = serializers.IntegerField()


class MerchantCreatedSerializer(serializers.Serializer[Any]):
    merchant_id = serializers.CharField()
    api_key = serializers.CharField(help_text="Shown once. Only a hash is stored.")
    stores = StoreSerializer(many=True)


class CourierCreateSerializer(serializers.Serializer[dict[str, Any]]):
    name = serializers.CharField(max_length=200)
    vehicle = serializers.ChoiceField(choices=Courier.Vehicle.choices)


class CourierTokenSerializer(serializers.Serializer[Any]):
    courier_id = serializers.CharField()
    token = serializers.CharField(help_text="Courier JWT (HS256), valid COURIER_TOKEN_TTL_SECONDS.")
