"""Django settings, read from environment variables only (12-factor).

Secrets have no defaults: the process refuses to start without them, listing every missing variable
at once. Non-secret settings have local-development defaults.
"""

from __future__ import annotations

import os

import dj_database_url
from cryptography.fernet import Fernet
from django.core.exceptions import ImproperlyConfigured

_missing: list[str] = []


def _secret(name: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        _missing.append(name)
    return value


def _int(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise ImproperlyConfigured(f"{name} must be an integer, got {raw!r}") from exc


def _bool(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    return default if raw is None else raw.strip().lower() in {"1", "true", "yes", "on"}


def _list(name: str, default: str) -> list[str]:
    return [item.strip() for item in os.environ.get(name, default).split(",") if item.strip()]


APP_VERSION = os.environ.get("APP_VERSION", "0.0.0")

# ---- Django core -------------------------------------------------------------------------------
SECRET_KEY = _secret("DJANGO_SECRET_KEY")
DEBUG = _bool("DJANGO_DEBUG", default=False)
# Probes and Prometheus reach pods by IP, so /healthz, /readyz and /metrics are answered before the
# Host check (see core.middleware). Every other request must carry one of these hosts.
ALLOWED_HOSTS = _list("DJANGO_ALLOWED_HOSTS", "localhost,127.0.0.1,[::1]")

INSTALLED_APPS = [
    "rest_framework",
    "drf_spectacular",
    "accounts",
    "orders",
    "messaging",
]

# API only: no sessions, cookies, CSRF or templates. Callers authenticate with bearer tokens.
MIDDLEWARE = [
    "core.middleware.RequestContextMiddleware",
    "core.middleware.OpsEndpointsMiddleware",
    "django.middleware.security.SecurityMiddleware",
    "django.middleware.common.CommonMiddleware",
    "django.middleware.clickjacking.XFrameOptionsMiddleware",
    # Innermost, so it sees the views' exceptions first: PostgreSQL unavailable -> 503 Retry-After.
    "core.middleware.DatabaseUnavailableMiddleware",
]

ROOT_URLCONF = "config.urls"
WSGI_APPLICATION = "config.wsgi.application"
APPEND_SLASH = False  # never redirect a POST; clients use the exact paths in openapi.yaml

USE_TZ = True
TIME_ZONE = "UTC"  # stored and returned in UTC; clients render JST themselves
LANGUAGE_CODE = "en"
USE_I18N = False

DEFAULT_AUTO_FIELD = "django.db.models.BigAutoField"

# Requests are a few hundred bytes; refuse anything large before parsing it.
DATA_UPLOAD_MAX_MEMORY_SIZE = 64 * 1024

# ---- Database ----------------------------------------------------------------------------------
DATABASES = {
    "default": dj_database_url.parse(
        # The placeholder only lets parsing finish; a missing DATABASE_URL stops startup below.
        _secret("DATABASE_URL") or "sqlite://:memory:",
        conn_max_age=_int("DB_CONN_MAX_AGE", 60),
        conn_health_checks=True,
    )
}
if DATABASES["default"]["ENGINE"] == "django.db.backends.postgresql":
    _db_options = DATABASES["default"].setdefault("OPTIONS", {})
    # A runaway query must not hold a worker (or locks) forever.
    _db_options["options"] = f"-c statement_timeout={_int('DB_STATEMENT_TIMEOUT_MS', 5000)}"
    # Give up on a server that does not answer the connection (psycopg waits 130 s by default), so
    # requests during a failover get their 503 quickly instead of holding a worker thread.
    _db_options["connect_timeout"] = _int("DB_CONNECT_TIMEOUT_SECONDS", 3)

# While PostgreSQL is unavailable, requests answer 503 database_unavailable with this Retry-After.
DB_RETRY_AFTER_SECONDS = _int("DB_RETRY_AFTER_SECONDS", 2)
# /readyz fails on the database only once it has failed for this long (core.database). 0 makes it
# fail at once. 120 s covers design §10's 1-2 minute failover.
READINESS_DB_GRACE_SECONDS = _int("READINESS_DB_GRACE_SECONDS", 120)

# ---- Security ----------------------------------------------------------------------------------
# TLS ends at the load balancer; these keep responses safe if they are ever rendered by a browser.
SECURE_CONTENT_TYPE_NOSNIFF = True
SECURE_REFERRER_POLICY = "no-referrer"
SECURE_CROSS_ORIGIN_OPENER_POLICY = "same-origin"
X_FRAME_OPTIONS = "DENY"
SESSION_COOKIE_SECURE = True
CSRF_COOKIE_SECURE = True
SILENCED_SYSTEM_CHECKS = [
    "security.W003",  # no CSRF middleware: bearer tokens only, no cookies or sessions to forge with
    "security.W004",  # HSTS is set by the ingress, which terminates TLS
    "security.W008",  # the ingress redirects to HTTPS; pods speak plain HTTP inside the cluster
]

# ---- Logging: one JSON object per line on stdout ----------------------------------------------
LOGGING = {
    "version": 1,
    "disable_existing_loggers": False,
    "filters": {"request_id": {"()": "core.logs.RequestIdFilter"}},
    "formatters": {"json": {"()": "core.logs.JsonFormatter"}},
    "handlers": {
        "stdout": {
            "class": "logging.StreamHandler",
            "stream": "ext://sys.stdout",
            "formatter": "json",
            "filters": ["request_id"],
        }
    },
    "root": {"handlers": ["stdout"], "level": os.environ.get("LOG_LEVEL", "INFO").upper()},
    "loggers": {
        # Our middleware writes one access line per request; Django's 4xx warnings would repeat it.
        "django.request": {"level": "ERROR"},
        # httpx logs request URLs at INFO, and the dispatch ETA URL carries drop-off coordinates.
        "httpx": {"level": "WARNING"},
        "httpcore": {"level": "WARNING"},
    },
}

# ---- Django REST framework and OpenAPI --------------------------------------------------------
REST_FRAMEWORK = {
    "DEFAULT_RENDERER_CLASSES": ["rest_framework.renderers.JSONRenderer"],
    "DEFAULT_PARSER_CLASSES": ["rest_framework.parsers.JSONParser"],
    # Each view names its own authentication (merchant key, courier JWT, admin token).
    "DEFAULT_AUTHENTICATION_CLASSES": [],
    # Deny by default: a view without authentication is unreachable, not public.
    "DEFAULT_PERMISSION_CLASSES": ["rest_framework.permissions.IsAuthenticated"],
    "UNAUTHENTICATED_USER": None,
    "EXCEPTION_HANDLER": "core.errors.exception_handler",
    "DEFAULT_SCHEMA_CLASS": "drf_spectacular.openapi.AutoSchema",
}

SPECTACULAR_SETTINGS = {
    "TITLE": "Tasuki orders API",
    "DESCRIPTION": (
        "Quotes, orders and the order lifecycle for Tasuki merchants and couriers. "
        'Errors are returned as {"error": {"code", "message"}}.'
    ),
    "VERSION": "1.0.0",
    "SERVE_INCLUDE_SCHEMA": False,
    "COMPONENT_SPLIT_REQUEST": True,
    "ENUM_NAME_OVERRIDES": {
        "OrderStatusEnum": "orders.state_machine.OrderStatus",
    },
    "POSTPROCESSING_HOOKS": [
        "drf_spectacular.hooks.postprocess_schema_enums",  # the default hook, kept
        "core.openapi.document_database_unavailable",
    ],
}

# ---- Tasuki ------------------------------------------------------------------------------------
# PII field encryption. Comma-separated Fernet keys: the first encrypts, all decrypt (key rotation).
# Production uses AWS KMS envelope encryption instead (see core/crypto.py).
ORDERS_FIELD_KEYS = _list("ORDERS_FIELD_KEY", "")
if not ORDERS_FIELD_KEYS:
    _missing.append("ORDERS_FIELD_KEY")

COURIER_TOKEN_SECRET = _secret("COURIER_TOKEN_SECRET")
COURIER_TOKEN_TTL_SECONDS = _int("COURIER_TOKEN_TTL_SECONDS", 24 * 3600)
TRACKING_TOKEN_SECRET = _secret("TRACKING_TOKEN_SECRET")
# Base URL of the customer tracking page (the console): tracking_url = this + /track/<JWT>.
# The page then streams from the tracking service itself.
TRACKING_PAGE_URL = os.environ.get("TRACKING_PAGE_URL", "http://localhost:8088").rstrip("/")
# Seeding/admin API. Empty means the admin endpoints are switched off (every call gets 401).
ADMIN_TOKEN = os.environ.get("ADMIN_TOKEN", "")

DISPATCH_URL = os.environ.get("DISPATCH_URL", "http://127.0.0.1:8082").rstrip("/")
DISPATCH_TIMEOUT_MS = _int("DISPATCH_TIMEOUT_MS", 300)

QUOTE_TTL_SECONDS = _int("QUOTE_TTL_SECONDS", 120)
IDEMPOTENCY_TTL_HOURS = _int("IDEMPOTENCY_TTL_HOURS", 24)
# NoCourierAvailable keeps an order dispatching until it is this old; then it is cancelled.
NO_COURIER_CANCEL_AFTER_SECONDS = _int("NO_COURIER_CANCEL_AFTER_SECONDS", 15 * 60)
DELIVERY_PROOF_RADIUS_M = _int("DELIVERY_PROOF_RADIUS_M", 500)

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092")
KAFKA_ORDERS_TOPIC = os.environ.get("KAFKA_ORDERS_TOPIC", "orders.events")
KAFKA_DISPATCH_TOPIC = os.environ.get("KAFKA_DISPATCH_TOPIC", "dispatch.events")
KAFKA_CONSUMER_GROUP = os.environ.get("KAFKA_CONSUMER_GROUP", "orders")
# The relay and the consumer are not web processes; they serve /metrics on this port.
WORKER_METRICS_PORT = _int("WORKER_METRICS_PORT", 9100)

if _missing:
    raise ImproperlyConfigured(
        "Missing required environment variables: " + ", ".join(sorted(set(_missing)))
    )

for _key in ORDERS_FIELD_KEYS:
    try:
        Fernet(_key)
    except ValueError as exc:
        raise ImproperlyConfigured(
            "ORDERS_FIELD_KEY must be 32 url-safe base64-encoded bytes. Generate one with: "
            "python -c 'from cryptography.fernet import Fernet; "
            "print(Fernet.generate_key().decode())'"
        ) from exc

for _name, _value in {
    "COURIER_TOKEN_SECRET": COURIER_TOKEN_SECRET,
    "TRACKING_TOKEN_SECRET": TRACKING_TOKEN_SECRET,
}.items():
    if len(_value) < 32:
        raise ImproperlyConfigured(f"{_name} must be at least 32 characters (HS256 key)")
