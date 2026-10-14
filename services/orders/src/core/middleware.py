"""Request context (request ID, access log, RED metrics), the operational endpoints, and the 503
for a database outage."""

from __future__ import annotations

import logging
import re
import time
import uuid
from collections.abc import Callable

from django.conf import settings
from django.db import Error, connection
from django.http import HttpRequest, HttpResponse, JsonResponse
from django.http.response import HttpResponseBase

from core import lifecycle
from core.database import DatabaseReadiness, unavailable_reason
from core.errors import database_unavailable
from core.logs import request_id_var
from core.metrics import DATABASE_UNAVAILABLE, HTTP_LATENCY, HTTP_REQUESTS, render_latest

log = logging.getLogger("orders.http")

Handler = Callable[[HttpRequest], HttpResponseBase]

OPS_PATHS = frozenset({"/healthz", "/readyz", "/metrics"})
_KNOWN_METHODS = frozenset({"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"})
# Accept a caller's request ID only if it is short and plain, so it can't inject into log lines.
_REQUEST_ID = re.compile(r"[A-Za-z0-9._-]{1,64}")


def _route(request: HttpRequest) -> str:
    """A low-cardinality route label: the URL pattern, never the raw path (IDs would explode it)."""
    if request.path in OPS_PATHS:
        return request.path
    match = request.resolver_match
    return f"/{match.route}" if match is not None else "unmatched"


class RequestContextMiddleware:
    """Outermost: request ID, one JSON access line per request, request count and latency."""

    def __init__(self, get_response: Handler) -> None:
        self.get_response = get_response

    def __call__(self, request: HttpRequest) -> HttpResponseBase:
        incoming = request.headers.get("X-Request-ID", "")
        request_id = incoming if _REQUEST_ID.fullmatch(incoming) else uuid.uuid4().hex
        token = request_id_var.set(request_id)
        start = time.perf_counter()
        try:
            response = self.get_response(request)
            elapsed = time.perf_counter() - start
            method = request.method if request.method in _KNOWN_METHODS else "OTHER"
            route = _route(request)
            HTTP_REQUESTS.labels(method, route, str(response.status_code)).inc()
            HTTP_LATENCY.labels(method, route).observe(elapsed)
            response["X-Request-ID"] = request_id
            if route not in OPS_PATHS:  # probes and scrapes every few seconds would drown the log
                log.info(
                    "request",
                    extra={
                        "method": method,
                        "route": route,
                        "status": response.status_code,
                        "duration_ms": round(elapsed * 1000, 2),
                    },
                )
            return response
        finally:
            request_id_var.reset(token)


class OpsEndpointsMiddleware:
    """/healthz, /readyz and /metrics, answered before Django's Host-header check.

    The kubelet and Prometheus call pods by IP address, which is not (and should not be) in
    ALLOWED_HOSTS. Every other path goes on to CommonMiddleware, which enforces ALLOWED_HOSTS.
    """

    def __init__(self, get_response: Handler) -> None:
        self.get_response = get_response

    def __call__(self, request: HttpRequest) -> HttpResponseBase:
        if request.method in ("GET", "HEAD"):
            if request.path == "/healthz":
                # Liveness: the process answers. Never checks dependencies (that causes restart
                # storms).
                return JsonResponse({"status": "ok"})
            if request.path == "/readyz":
                return readiness()
            if request.path == "/metrics":
                body, content_type = render_latest()
                return HttpResponse(body, content_type=content_type)
        return self.get_response(request)


# Per process: each gunicorn worker keeps its own clock (see DatabaseReadiness).
database_readiness = DatabaseReadiness(grace_s=settings.READINESS_DB_GRACE_SECONDS)


def readiness() -> JsonResponse:
    """Ready = not shutting down, and the database answers or has failed for less than the grace.

    503 during shutdown drains traffic. A short database outage keeps the pod ready on purpose
    (core.database.DatabaseReadiness), so its requests answer 503 + Retry-After and are counted.
    """
    if lifecycle.is_shutting_down():
        return JsonResponse({"status": "shutting_down"}, status=503)
    answered = _database_answers()
    verdict = database_readiness.record(answered)
    if answered:
        return JsonResponse({"status": "ready"})
    failing_for_s = int(verdict.failing_for_s)
    log.warning(
        "readiness check: database unreachable",
        extra={"failing_for_s": failing_for_s, "ready": verdict.ready},
    )
    if verdict.ready:
        return JsonResponse({"status": "degraded", "database_failing_for_s": failing_for_s})
    return JsonResponse({"status": "database_unreachable"}, status=503)


def _database_answers() -> bool:
    try:
        with connection.cursor() as cursor:
            cursor.execute("SELECT 1")
    except Error:  # every driver error, including InterfaceError (not a DatabaseError)
        return False
    return True


class DatabaseUnavailableMiddleware:
    """Innermost: a view that failed because PostgreSQL is unavailable answers 503 + Retry-After.

    By the time the exception gets here, transaction.atomic has rolled back what the request wrote,
    so the client can send the same request again. For POST /v1/orders with the same
    Idempotency-Key, the retry replays the stored 201 if the commit reached the server after all
    (the client cannot tell), or creates the order once if it did not (orders/idempotency.py).

    DRF's exception handler passes database errors on (core.errors), so they arrive here. An error
    that core.database does not call an outage is left to Django's usual 500.
    """

    def __init__(self, get_response: Handler) -> None:
        self.get_response = get_response

    def __call__(self, request: HttpRequest) -> HttpResponseBase:
        return self.get_response(request)

    def process_exception(
        self, request: HttpRequest, exception: Exception
    ) -> HttpResponseBase | None:
        reason = unavailable_reason(exception)
        if reason is None:
            return None
        route = _route(request)
        DATABASE_UNAVAILABLE.labels(route, reason).inc()
        cause = exception.__cause__ or exception  # psycopg's own error names the problem best
        # First line only (libpq adds hints below). Connection-level messages name a host and port
        # at most, never row data.
        message = str(cause).partition("\n")[0][:200]
        log.warning(
            "database unavailable",
            extra={"route": route, "reason": reason, "error": f"{type(cause).__name__}: {message}"},
        )
        return database_unavailable(settings.DB_RETRY_AFTER_SECONDS)
