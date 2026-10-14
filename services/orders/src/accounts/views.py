"""Admin/seeding API: create merchants (with stores and an API key) and couriers (with a token)."""

from __future__ import annotations

import logging

from django.db import transaction
from django.utils import timezone
from drf_spectacular.utils import extend_schema
from rest_framework.request import Request
from rest_framework.response import Response
from rest_framework.views import APIView

from accounts.auth import AdminTokenAuthentication
from accounts.models import ApiKey, Courier, Merchant, Store
from accounts.serializers import (
    CourierCreateSerializer,
    CourierTokenSerializer,
    MerchantCreatedSerializer,
    MerchantCreateSerializer,
    StoreSerializer,
)
from core.errors import NotFoundError
from core.openapi import INVALID, UNAUTHENTICATED, error
from core.tokens import issue_courier_token

log = logging.getLogger("orders.admin")


class AdminView(APIView):
    authentication_classes = (AdminTokenAuthentication,)


class MerchantCreateView(AdminView):
    @extend_schema(
        operation_id="admin_create_merchant",
        tags=["admin"],
        request=MerchantCreateSerializer,
        responses={201: MerchantCreatedSerializer, 400: INVALID, 401: UNAUTHENTICATED},
    )
    def post(self, request: Request) -> Response:
        body = MerchantCreateSerializer(data=request.data)
        body.is_valid(raise_exception=True)
        with transaction.atomic():
            merchant = Merchant.objects.create(name=body.validated_data["name"])
            _, api_key = ApiKey.issue(merchant)
            stores = Store.objects.bulk_create(
                Store(merchant=merchant, **store) for store in body.validated_data["stores"]
            )
        log.info("merchant created", extra={"merchant_id": merchant.id, "stores": len(stores)})
        data = {
            "merchant_id": merchant.id,
            "api_key": api_key,
            "stores": StoreSerializer(stores, many=True).data,
        }
        return Response(data, status=201)


class CourierCreateView(AdminView):
    @extend_schema(
        operation_id="admin_create_courier",
        tags=["admin"],
        request=CourierCreateSerializer,
        responses={201: CourierTokenSerializer, 400: INVALID, 401: UNAUTHENTICATED},
    )
    def post(self, request: Request) -> Response:
        body = CourierCreateSerializer(data=request.data)
        body.is_valid(raise_exception=True)
        courier = Courier.objects.create(**body.validated_data)
        log.info("courier created", extra={"courier_id": courier.id})
        token = issue_courier_token(courier.id, timezone.now())
        return Response({"courier_id": courier.id, "token": token}, status=201)


class CourierTokenView(AdminView):
    @extend_schema(
        operation_id="admin_issue_courier_token",
        tags=["admin"],
        request=None,
        responses={200: CourierTokenSerializer, 401: UNAUTHENTICATED, 404: error("not_found")},
    )
    def post(self, request: Request, courier_id: str) -> Response:
        """Issue a fresh token for an existing courier (tokens are not refreshable)."""
        courier = Courier.objects.filter(pk=courier_id, is_active=True).first()
        if courier is None:
            raise NotFoundError("No such courier.")
        token = issue_courier_token(courier.id, timezone.now())
        return Response({"courier_id": courier.id, "token": token})
