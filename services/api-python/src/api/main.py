"""HTTP API: health and readiness probes, Prometheus metrics, request IDs and JSON access logs."""

from __future__ import annotations

import logging
import time
import uuid
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from typing import Annotated

from fastapi import FastAPI, Query, Request, Response
from fastapi.responses import JSONResponse
from prometheus_client import (
    CONTENT_TYPE_LATEST,
    CollectorRegistry,
    Counter,
    Histogram,
    generate_latest,
)

from api.config import Settings, load_settings
from api.logs import configure_logging

log = logging.getLogger("api")


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or load_settings()
    configure_logging(settings.log_level)

    # A registry per app keeps tests independent and avoids duplicate-metric errors on reload.
    registry = CollectorRegistry()
    requests_total = Counter(
        "http_requests_total", "HTTP requests.", ["method", "route", "status"], registry=registry
    )
    latency = Histogram(
        "http_request_duration_seconds",
        "HTTP request latency.",
        ["method", "route"],
        registry=registry,
    )
    state = {"ready": False}

    @asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        state["ready"] = True
        log.info("startup", extra={"service": settings.service_name, "version": settings.version})
        yield
        # Fail readiness first so the load balancer stops sending traffic,
        # then let in-flight requests finish.
        state["ready"] = False
        log.info("shutdown")

    app = FastAPI(title=settings.service_name, version=settings.version, lifespan=lifespan)

    @app.middleware("http")
    async def observe(
        request: Request, call_next: Callable[[Request], Awaitable[Response]]
    ) -> Response:
        request_id = request.headers.get("x-request-id") or uuid.uuid4().hex
        start = time.perf_counter()
        response = await call_next(request)
        elapsed = time.perf_counter() - start
        # Label by path only for matched routes, so random 404 URLs can't explode
        # metric cardinality. When you add path parameters, label with the route template.
        route = request.url.path if response.status_code != 404 else "unmatched"
        requests_total.labels(request.method, route, str(response.status_code)).inc()
        latency.labels(request.method, route).observe(elapsed)
        response.headers["x-request-id"] = request_id
        log.info(
            "request",
            extra={
                "request_id": request_id,
                "method": request.method,
                "route": route,
                "status": response.status_code,
                "duration_ms": round(elapsed * 1000, 2),
            },
        )
        return response

    @app.get("/healthz")
    async def healthz() -> dict[str, str]:
        """Liveness: the process is up. Never checks dependencies (that causes restart storms)."""
        return {"status": "ok"}

    @app.get("/readyz")
    async def readyz() -> JSONResponse:
        """Readiness: safe to receive traffic. Add dependency checks (DB, cache) here."""
        if state["ready"]:
            return JSONResponse({"status": "ready"})
        return JSONResponse({"status": "not ready"}, status_code=503)

    @app.get("/metrics")
    async def metrics() -> Response:
        return Response(generate_latest(registry), media_type=CONTENT_TYPE_LATEST)

    @app.get("/v1/hello")
    async def hello(
        name: Annotated[str, Query(min_length=1, max_length=64)] = "world",
    ) -> dict[str, str]:
        return {"message": f"Hello, {name}!"}

    return app


app = create_app()
