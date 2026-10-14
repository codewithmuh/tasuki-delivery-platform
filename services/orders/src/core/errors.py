"""Errors. Every error body is {"error": {"code": "snake_case", "message": "..."}} (contracts/).

Domain code raises DomainError subclasses and stays independent of DRF; the exception handler below
turns them, and DRF's own exceptions, into that shape.
"""

from __future__ import annotations

from typing import Any, ClassVar

from django.http import HttpRequest, JsonResponse
from rest_framework import exceptions
from rest_framework.response import Response
from rest_framework.views import exception_handler as drf_exception_handler


class DomainError(Exception):
    """A business-rule failure with a stable machine-readable code and an HTTP status."""

    status: ClassVar[int] = 400
    code: ClassVar[str] = "bad_request"
    default_message: ClassVar[str] = "The request cannot be processed."

    def __init__(self, message: str | None = None) -> None:
        self.message = message or self.default_message
        super().__init__(self.message)


class NotFoundError(DomainError):
    status = 404
    code = "not_found"
    default_message = "Not found."


class ConflictError(DomainError):
    """The request is valid but conflicts with the resource's current state."""

    status = 409
    code = "conflict"


class UnprocessableError(DomainError):
    status = 422
    code = "unprocessable"


def error_body(code: str, message: str, **extra: Any) -> dict[str, Any]:
    return {"error": {"code": code, "message": message, **extra}}


def exception_handler(exc: Exception, context: dict[str, Any]) -> Response | None:
    if isinstance(exc, DomainError):
        return Response(error_body(exc.code, exc.message), status=exc.status)
    response = drf_exception_handler(exc, context)
    if response is None:
        return None  # not an API error: Django logs it and answers with handler500
    if isinstance(exc, exceptions.ValidationError):
        response.data = error_body(
            "validation_error", "The request body is invalid.", details=response.data
        )
    elif isinstance(exc, exceptions.APIException):
        codes = exc.get_codes()
        code = codes if isinstance(codes, str) else exc.default_code
        message = str(exc.detail) if isinstance(exc.detail, str) else str(exc.default_detail)
        response.data = error_body(code, message)
    return response


def bad_request(request: HttpRequest, exception: Exception | None = None) -> JsonResponse:
    # Also what a request with a Host header outside ALLOWED_HOSTS gets.
    return JsonResponse(error_body("bad_request", "Bad request."), status=400)


def permission_denied(request: HttpRequest, exception: Exception | None = None) -> JsonResponse:
    return JsonResponse(error_body("forbidden", "Forbidden."), status=403)


def not_found(request: HttpRequest, exception: Exception | None = None) -> JsonResponse:
    return JsonResponse(error_body("not_found", "No such endpoint."), status=404)


def server_error(request: HttpRequest) -> JsonResponse:
    return JsonResponse(
        error_body("internal_error", "Something went wrong on our side."), status=500
    )
