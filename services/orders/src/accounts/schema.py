"""OpenAPI security schemes for the custom authentication classes (drf-spectacular extensions)."""

from __future__ import annotations

from typing import Any

from drf_spectacular.extensions import OpenApiAuthenticationExtension


class MerchantKeyScheme(OpenApiAuthenticationExtension):
    target_class = "accounts.auth.MerchantKeyAuthentication"
    name = "merchantApiKey"

    def get_security_definition(self, auto_schema: Any) -> dict[str, str]:
        return {
            "type": "http",
            "scheme": "bearer",
            "description": "Merchant API key (`tsk_…`), issued by POST /v1/admin/merchants.",
        }


class CourierTokenScheme(OpenApiAuthenticationExtension):
    target_class = "accounts.auth.CourierTokenAuthentication"
    name = "courierToken"

    def get_security_definition(self, auto_schema: Any) -> dict[str, str]:
        return {
            "type": "http",
            "scheme": "bearer",
            "bearerFormat": "JWT",
            "description": "Courier JWT (HS256; claims sub, role=courier, exp).",
        }


class AdminTokenScheme(OpenApiAuthenticationExtension):
    target_class = "accounts.auth.AdminTokenAuthentication"
    name = "adminToken"

    def get_security_definition(self, auto_schema: Any) -> dict[str, str]:
        return {
            "type": "http",
            "scheme": "bearer",
            "description": "ADMIN_TOKEN, for seeding. Not enabled in production.",
        }
