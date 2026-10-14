"""OpenAPI pieces shared by the apps: the error body and a helper to document error responses."""

from __future__ import annotations

import copy
from typing import Any

from drf_spectacular.utils import OpenApiResponse
from rest_framework import serializers


class ErrorDetailSerializer(serializers.Serializer[Any]):
    code = serializers.CharField(
        help_text="Stable, snake_case; branch on this, not on the message."
    )
    message = serializers.CharField()
    details = serializers.DictField(required=False, help_text="Field errors (validation_error).")


class ErrorSerializer(serializers.Serializer[Any]):
    error = ErrorDetailSerializer()


def error(codes: str) -> OpenApiResponse:
    return OpenApiResponse(response=ErrorSerializer, description=f"Error codes: {codes}.")


UNAUTHENTICATED = error("not_authenticated, authentication_failed")
INVALID = error("validation_error, parse_error")

_HTTP_METHODS = ("get", "put", "post", "delete", "patch")

# core.middleware.DatabaseUnavailableMiddleware can answer this on any operation.
DATABASE_UNAVAILABLE_RESPONSE: dict[str, Any] = {
    "description": (
        "Error codes: database_unavailable. The database is failing over or restarting: retry "
        "after Retry-After seconds. Order creation is safe to retry with the same Idempotency-Key."
    ),
    "headers": {
        "Retry-After": {
            "description": "Seconds to wait before retrying.",
            "schema": {"type": "integer"},
        }
    },
    "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}},
}


def document_database_unavailable(
    result: dict[str, Any], generator: Any, **kwargs: Any
) -> dict[str, Any]:
    """drf-spectacular postprocessing hook: add the 503 to every operation, in one place."""
    for path_item in result.get("paths", {}).values():
        for method, operation in path_item.items():
            if method in _HTTP_METHODS:
                operation["responses"]["503"] = copy.deepcopy(DATABASE_UNAVAILABLE_RESPONSE)
    return result
