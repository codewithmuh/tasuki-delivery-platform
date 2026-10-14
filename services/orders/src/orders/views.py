"""HTTP layer for quotes and orders: parse, authorise, call orders.services, render."""

from __future__ import annotations

from typing import Any

from drf_spectacular.utils import OpenApiParameter, extend_schema
from rest_framework.request import Request
from rest_framework.response import Response
from rest_framework.views import APIView

from accounts.auth import (
    CourierTokenAuthentication,
    MerchantKeyAuthentication,
    current_courier,
    current_merchant,
)
from accounts.models import Store
from core.errors import NotFoundError
from core.metrics import IDEMPOTENT_REPLAYS
from core.openapi import INVALID, UNAUTHENTICATED, error
from orders import idempotency, services
from orders.models import Order
from orders.serializers import (
    CancelSerializer,
    CourierOrderSerializer,
    DeliverSerializer,
    FailSerializer,
    OrderCreatedSerializer,
    OrderCreateSerializer,
    OrderDetailSerializer,
    QuoteRequestSerializer,
    QuoteSerializer,
)


class StoreNotFoundError(NotFoundError):
    code = "store_not_found"
    default_message = "No such store."


class MerchantView(APIView):
    authentication_classes = (MerchantKeyAuthentication,)


class CourierView(APIView):
    authentication_classes = (CourierTokenAuthentication,)


class QuoteCreateView(MerchantView):
    @extend_schema(
        operation_id="create_quote",
        tags=["quotes"],
        request=QuoteRequestSerializer,
        responses={
            201: QuoteSerializer,
            400: INVALID,
            401: UNAUTHENTICATED,
            404: error("store_not_found"),
        },
    )
    def post(self, request: Request) -> Response:
        """Price and ETA for one delivery, valid for 2 minutes."""
        merchant = current_merchant(request)
        body = QuoteRequestSerializer(data=request.data)
        body.is_valid(raise_exception=True)
        store = Store.objects.filter(merchant=merchant, pk=body.validated_data["store_id"]).first()
        if store is None:
            raise StoreNotFoundError()
        dropoff = services.Dropoff(**body.validated_data["dropoff"])
        quote = services.create_quote(merchant, store, dropoff)
        return Response(QuoteSerializer(quote).data, status=201)


class OrderCreateView(MerchantView):
    @extend_schema(
        operation_id="create_order",
        tags=["orders"],
        request=OrderCreateSerializer,
        parameters=[
            OpenApiParameter(
                idempotency.HEADER,
                location=OpenApiParameter.HEADER,
                required=True,
                description="Unique per order attempt (a UUID). Retries reuse it; kept 24 h.",
            )
        ],
        responses={
            201: OrderCreatedSerializer,
            400: error("idempotency_key_required, validation_error"),
            401: UNAUTHENTICATED,
            404: error("quote_not_found"),
            409: error("quote_expired, quote_already_used"),
            422: error("idempotency_key_reused, dropoff_mismatch"),
        },
    )
    def post(self, request: Request) -> Response:
        """Create a confirmed order from a quote. Safe to retry with the same Idempotency-Key."""
        merchant = current_merchant(request)
        key = idempotency.validate_key(request.headers.get(idempotency.HEADER))
        body = OrderCreateSerializer(data=request.data)
        body.is_valid(raise_exception=True)
        dropoff: services.Dropoff | None = None
        if "dropoff" in body.validated_data:
            dropoff = services.Dropoff(**body.validated_data["dropoff"])

        def create() -> tuple[int, dict[str, Any]]:
            order = services.create_order(merchant, body.validated_data["quote_id"], dropoff)
            return 201, dict(OrderCreatedSerializer(order).data)

        fingerprint = idempotency.request_hash(request.method or "POST", request.path, request.data)
        result = idempotency.run_once(merchant, key, fingerprint, create)
        response = Response(result.body, status=result.status)
        if result.replayed:
            IDEMPOTENT_REPLAYS.inc()
            response["Idempotent-Replayed"] = "true"
        return response


def _merchant_order(request: Request, order_id: str) -> Order:
    merchant = current_merchant(request)
    # Scoped to the caller: another merchant's order is "not found", exactly like a missing one.
    order = Order.objects.filter(merchant=merchant, pk=order_id).prefetch_related("history").first()
    if order is None:
        raise services.OrderNotFoundError()
    return order


class OrderDetailView(MerchantView):
    @extend_schema(
        operation_id="get_order",
        tags=["orders"],
        responses={200: OrderDetailSerializer, 401: UNAUTHENTICATED, 404: error("not_found")},
    )
    def get(self, request: Request, order_id: str) -> Response:
        """The order and its status history, oldest first."""
        return Response(OrderDetailSerializer(_merchant_order(request, order_id)).data)


class OrderCancelView(MerchantView):
    @extend_schema(
        operation_id="cancel_order",
        tags=["orders"],
        request=CancelSerializer,
        responses={
            200: OrderDetailSerializer,
            400: INVALID,
            401: UNAUTHENTICATED,
            404: error("not_found"),
            409: error("illegal_transition"),
        },
    )
    def post(self, request: Request, order_id: str) -> Response:
        """Cancel before pickup. After pickup the order can only be delivered or fail (409)."""
        merchant = current_merchant(request)
        body = CancelSerializer(data=request.data)
        body.is_valid(raise_exception=True)
        services.cancel_order(merchant, order_id, body.validated_data["reason"])
        return Response(OrderDetailSerializer(_merchant_order(request, order_id)).data)


class PickupView(CourierView):
    @extend_schema(
        operation_id="courier_pickup",
        tags=["courier"],
        request=None,
        responses={
            200: CourierOrderSerializer,
            401: UNAUTHENTICATED,
            404: error("not_found (not assigned to you)"),
            409: error("illegal_transition"),
        },
    )
    def post(self, request: Request, order_id: str) -> Response:
        """The assigned courier has the order. 404 if it is not assigned to the caller."""
        order = services.pick_up(current_courier(request), order_id)
        return Response(CourierOrderSerializer(order).data)


class DeliverView(CourierView):
    @extend_schema(
        operation_id="courier_deliver",
        tags=["courier"],
        request=DeliverSerializer,
        responses={
            200: CourierOrderSerializer,
            400: INVALID,
            401: UNAUTHENTICATED,
            404: error("not_found (not assigned to you)"),
            409: error("illegal_transition"),
            422: error("proof_too_far"),
        },
    )
    def post(self, request: Request, order_id: str) -> Response:
        """Delivered. The courier's position is the proof and must be near the drop-off."""
        body = DeliverSerializer(data=request.data)
        body.is_valid(raise_exception=True)
        order = services.deliver(
            current_courier(request),
            order_id,
            body.validated_data["lat"],
            body.validated_data["lng"],
        )
        return Response(CourierOrderSerializer(order).data)


class FailView(CourierView):
    @extend_schema(
        operation_id="courier_fail",
        tags=["courier"],
        request=FailSerializer,
        responses={
            200: CourierOrderSerializer,
            401: UNAUTHENTICATED,
            404: error("not_found (not assigned to you)"),
            409: error("illegal_transition"),
        },
    )
    def post(self, request: Request, order_id: str) -> Response:
        """The courier has the order but cannot hand it over."""
        body = FailSerializer(data=request.data)
        body.is_valid(raise_exception=True)
        order = services.fail_delivery(
            current_courier(request), order_id, body.validated_data["reason"]
        )
        return Response(CourierOrderSerializer(order).data)
