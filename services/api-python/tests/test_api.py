import json
import logging
from collections.abc import Iterator

import pytest
from fastapi.testclient import TestClient

from api.config import Settings, load_settings
from api.logs import JsonFormatter
from api.main import create_app


@pytest.fixture
def client() -> Iterator[TestClient]:
    # Entering the context runs the lifespan, so the app reports ready.
    with TestClient(create_app(Settings(version="1.2.3"))) as c:
        yield c


def test_healthz(client: TestClient) -> None:
    r = client.get("/healthz")
    assert r.status_code == 200
    assert r.json() == {"status": "ok"}


def test_readyz_when_started(client: TestClient) -> None:
    assert client.get("/readyz").json() == {"status": "ready"}


def test_readyz_before_startup_is_503() -> None:
    # Without the context manager the lifespan never runs: the app must not claim readiness.
    r = TestClient(create_app()).get("/readyz")
    assert r.status_code == 503


@pytest.mark.parametrize(
    ("query", "expected"), [("", "Hello, world!"), ("?name=Tokyo", "Hello, Tokyo!")]
)
def test_hello(client: TestClient, query: str, expected: str) -> None:
    assert client.get(f"/v1/hello{query}").json() == {"message": expected}


def test_hello_rejects_long_names(client: TestClient) -> None:
    assert client.get("/v1/hello", params={"name": "x" * 65}).status_code == 422


def test_request_id_is_echoed_or_generated(client: TestClient) -> None:
    assert client.get("/healthz", headers={"x-request-id": "abc"}).headers["x-request-id"] == "abc"
    assert len(client.get("/healthz").headers["x-request-id"]) == 32


def test_metrics_count_requests(client: TestClient) -> None:
    client.get("/healthz")
    client.get("/does-not-exist")
    body = client.get("/metrics").text
    assert 'http_requests_total{method="GET",route="/healthz",status="200"}' in body
    assert 'route="unmatched"' in body


def test_json_formatter_includes_extra_fields() -> None:
    record = logging.makeLogRecord(
        {"msg": "hi", "levelname": "INFO", "name": "t", "request_id": "r1"}
    )
    out = json.loads(JsonFormatter().format(record))
    assert out["msg"] == "hi"
    assert out["request_id"] == "r1"
    assert out["level"] == "info"


def test_load_settings_from_env() -> None:
    s = load_settings({"SERVICE_NAME": "svc", "APP_VERSION": "2.0.0", "LOG_LEVEL": "debug"})
    assert s == Settings(service_name="svc", version="2.0.0", log_level="DEBUG")
