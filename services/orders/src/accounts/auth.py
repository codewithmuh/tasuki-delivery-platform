"""The three ways to call the API, each `Authorization: Bearer <token>`.

- Merchants: an API key (accounts.models.ApiKey). P3 replaces this with OAuth2 client credentials.
- Couriers: an HS256 JWT issued by this service (core.tokens) and also verified by the Go services.
- Admin/seeding: the ADMIN_TOKEN shared secret; unset means the admin API is off.

Each view lists exactly one of these. A view that lists none is unreachable (settings: deny by
default).
"""

from __future__ import annotations

import hmac
from dataclasses import dataclass
from typing import Any

from django.conf import settings
from rest_framework.authentication import BaseAuthentication, get_authorization_header
from rest_framework.exceptions import AuthenticationFailed, NotAuthenticated
from rest_framework.request import Request

from accounts.models import ApiKey, Courier, Merchant
from core.tokens import InvalidTokenError, verify_courier_token


def bearer_token(request: Request) -> str | None:
    scheme, _, token = get_authorization_header(request).decode("latin-1").partition(" ")
    if scheme.lower() != "bearer" or not token.strip():
        return None
    return token.strip()


class MerchantKeyAuthentication(BaseAuthentication):
    def authenticate(self, request: Request) -> tuple[Merchant, ApiKey] | None:
        token = bearer_token(request)
        if token is None:
            return None  # DRF answers 401 not_authenticated
        key = ApiKey.authenticate(token)
        if key is None:
            raise AuthenticationFailed("Invalid or revoked API key.", code="invalid_api_key")
        return key.merchant, key

    def authenticate_header(self, request: Request) -> str:
        return 'Bearer realm="merchant"'


class CourierTokenAuthentication(BaseAuthentication):
    def authenticate(self, request: Request) -> tuple[Courier, str] | None:
        token = bearer_token(request)
        if token is None:
            return None
        try:
            courier_id = verify_courier_token(token)
        except InvalidTokenError as exc:
            raise AuthenticationFailed(
                "Invalid or expired courier token.", code="invalid_token"
            ) from exc
        courier = Courier.objects.filter(pk=courier_id, is_active=True).first()
        if courier is None:
            raise AuthenticationFailed("Unknown or inactive courier.", code="invalid_token")
        return courier, token

    def authenticate_header(self, request: Request) -> str:
        return 'Bearer realm="courier"'


@dataclass(frozen=True)
class AdminPrincipal:
    is_authenticated: bool = True


class AdminTokenAuthentication(BaseAuthentication):
    def authenticate(self, request: Request) -> tuple[AdminPrincipal, None] | None:
        token = bearer_token(request)
        if token is None:
            return None
        expected: str = settings.ADMIN_TOKEN
        # compare_digest: the comparison takes the same time however many characters match.
        if not expected or not hmac.compare_digest(token.encode(), expected.encode()):
            raise AuthenticationFailed("Invalid admin token.", code="invalid_admin_token")
        return AdminPrincipal(), None

    def authenticate_header(self, request: Request) -> str:
        return 'Bearer realm="admin"'


def current_merchant(request: Request) -> Merchant:
    return _principal(request, Merchant)


def current_courier(request: Request) -> Courier:
    return _principal(request, Courier)


def _principal[T](request: Request, kind: type[T]) -> T:
    user: Any = request.user
    if not isinstance(user, kind):
        raise NotAuthenticated()
    return user
