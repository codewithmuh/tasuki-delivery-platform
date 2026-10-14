"""Request context (request ID, access log, RED metrics) and the operational endpoints."""

from __future__ import annotations

import logging
import re
import time
import uuid
from collections.abc import Callable

from django.db import DatabaseError, connection
from django.http import HttpRequest, HttpResponse, JsonResponse
from django.http.response import HttpResponseBase

from core import lifecycle
from core.logs import request_id_var
from core.metrics import HTTP_LATENCY, HTTP_REQUESTS, render_latest

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


def readiness() -> JsonResponse:
    """Ready = not shutting down, and the database answers. 503 during shutdown drains traffic."""
    if lifecycle.is_shutting_down():
        return JsonResponse({"status": "shutting_down"}, status=503)
    try:
        with connection.cursor() as cursor:
            cursor.execute("SELECT 1")
    except DatabaseError:
        log.warning("readiness check failed: database unreachable")
        return JsonResponse({"status": "database_unreachable"}, status=503)
    return JsonResponse({"status": "ready"})
