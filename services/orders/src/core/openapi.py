"""OpenAPI pieces shared by the apps: the error body and a helper to document error responses."""

from __future__ import annotations

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
