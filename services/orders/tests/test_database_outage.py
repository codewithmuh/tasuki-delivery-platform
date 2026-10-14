"""PostgreSQL unavailable (design §10, docs/gameday/experiments/06-postgres-failover.md).

Requests get 503 database_unavailable + Retry-After, nothing is half-written, and POST /v1/orders
retried with the same Idempotency-Key creates the order exactly once. /readyz stays ready through a
short outage, so those 503s reach the availability SLI.

SQLite cannot fail over, so most tests raise exactly what Django's PostgreSQL backend raises when
psycopg reports a failure (`django_error`), at a chosen SQL statement (`database_dies_at`) or at
COMMIT (`commit_fails`). Tests marked `real_transactions` run without the usual per-test wrapping
transaction, so the commits and rollbacks they exercise are real. Tests marked `postgres` make a
real server fail (`server_fails_before`) and are skipped on SQLite.
"""

from __future__ import annotations

import logging
from collections.abc import Callable, Iterator, Sequence
from contextlib import contextmanager
from types import SimpleNamespace
from typing import Any, cast
from unittest import mock

import psycopg
import pytest
from django.db import Error, InterfaceError, InternalError, connection, connections
from django.db.utils import DatabaseErrorWrapper
from drf_spectacular.generators import SchemaGenerator
from prometheus_client import REGISTRY
from rest_framework.test import APIClient

from core.database import DatabaseReadiness, ReadinessVerdict, unavailable_reason
from messaging.models import OutboxEvent
from orders.models import IdempotencyRecord, Order, OrderEvent, Quote
from tests.helpers import Tenant, dropoff, post_order, quote

real_transactions = pytest.mark.django_db(transaction=True)

CONNECTION_LOST = "server closed the connection unexpectedly"


def django_error(sqlstate: str | None, message: str = CONNECTION_LOST) -> Error:
    """What Django raises when psycopg reports `sqlstate` (None: the server never answered).

    Made by Django's own translation layer, as its PostgreSQL backend uses it: one of Django's
    exception classes, with psycopg's error as the __cause__.
    """
    cause = (
        psycopg.errors.lookup(sqlstate)(message) if sqlstate else psycopg.OperationalError(message)
    )
    translate = DatabaseErrorWrapper(cast(Any, SimpleNamespace(Database=psycopg)))
    try:
        with translate:
            raise cause
    except Error as translated:
        return translated
    raise AssertionError("Django did not translate the error")


@contextmanager
def database_dies_at(
    statement: str, sqlstate: str | None = None, message: str = CONNECTION_LOST
) -> Iterator[None]:
    """From the first SQL statement containing `statement` on, every statement fails.

    As if the server went away at that moment. Django commits and rolls back through the driver,
    not through a cursor, so the transaction still rolls back here (PostgreSQL rolls it back by
    itself when its client disappears).
    """
    dead = False

    def execute(
        run: Callable[..., Any], sql: str, params: Any, many: bool, context: dict[str, Any]
    ) -> Any:
        nonlocal dead
        dead = dead or statement in sql
        if dead:
            raise django_error(sqlstate, message)
        return run(sql, params, many, context)

    with connection.execute_wrapper(execute):
        yield


@contextmanager
def commit_fails(*, after_committing: bool) -> Iterator[None]:
    """The next COMMIT raises "server closed the connection unexpectedly".

    With after_committing, the server had committed before the connection broke. The client cannot
    tell the two apart, which is exactly why the retry must be safe either way.
    """
    db = connections["default"]
    commit = db._commit

    def broken_commit() -> None:
        if after_committing:
            commit()
        raise django_error(None)

    with mock.patch.object(db, "_commit", broken_commit):
        yield


@contextmanager
def server_fails_before(statement: str, failure: Sequence[str]) -> Iterator[None]:
    """PostgreSQL only: run the `failure` statements just before the first statement containing
    `statement`, so the real server fails the request there."""
    done = False

    def execute(
        run: Callable[..., Any], sql: str, params: Any, many: bool, context: dict[str, Any]
    ) -> Any:
        nonlocal done
        if not done and statement in sql:
            done = True
            for failing_sql in failure:
                run(failing_sql, None, False, context)
        return run(sql, params, many, context)

    with connection.execute_wrapper(execute):
        yield


@contextmanager
def counts_one_503(route: str, reason: str) -> Iterator[None]:
    """The block answers one 503, counted by both the outage metric and the SLI's metric."""
    samples = [
        ("orders_database_unavailable_total", {"route": route, "reason": reason}),
        ("http_requests_total", {"method": "POST", "route": route, "status": "503"}),
    ]
    before = [REGISTRY.get_sample_value(name, labels) or 0.0 for name, labels in samples]
    yield
    after = [REGISTRY.get_sample_value(name, labels) or 0.0 for name, labels in samples]
    assert after == [value + 1 for value in before]


def assert_database_unavailable(response: Any, retry_after: str = "2") -> None:
    assert response.status_code == 503
    assert response.headers["Retry-After"] == retry_after
    assert response.json()["error"]["code"] == "database_unavailable"


def assert_nothing_written() -> None:
    assert not Order.objects.exists()
    assert not OrderEvent.objects.exists()
    assert not OutboxEvent.objects.exists()
    assert not IdempotencyRecord.objects.exists()


def assert_one_order(body: dict[str, Any]) -> None:
    assert Order.objects.get().id == body["order_id"]
    assert OrderEvent.objects.count() == OutboxEvent.objects.count() == 1
    assert IdempotencyRecord.objects.count() == 1


# ---- which errors are outages --------------------------------------------------------------------


@pytest.mark.parametrize(
    ("sqlstate", "reason"),
    [
        (None, "connection"),  # refused, reset, timed out: the server never answered
        ("08006", "connection"),  # connection_failure
        ("57P01", "shutdown"),  # admin_shutdown: a switchover, or the primary pod deleted
        ("57P03", "shutdown"),  # cannot_connect_now: starting up or recovering
        ("25006", "read_only"),  # connected to a standby
        ("53300", "too_many_connections"),
        ("57014", "timeout"),  # DB_STATEMENT_TIMEOUT_MS ran out
    ],
)
def test_outages_are_recognised(sqlstate: str | None, reason: str) -> None:
    assert unavailable_reason(django_error(sqlstate)) == reason


@pytest.mark.parametrize(
    "sqlstate",
    [
        "40P01",  # deadlock_detected: an OperationalError too, but a bug in our locking
        "40001",  # serialization_failure
        "23505",  # unique_violation (IntegrityError)
        "42P01",  # undefined_table (ProgrammingError)
        "22001",  # string_data_right_truncation (DataError)
        "25P02",  # in_failed_sql_transaction (InternalError)
    ],
)
def test_other_database_errors_are_not_outages(sqlstate: str) -> None:
    assert unavailable_reason(django_error(sqlstate)) is None


def test_closed_connections_and_other_exceptions() -> None:
    assert unavailable_reason(InterfaceError("connection already closed")) == "connection"
    assert unavailable_reason(InternalError("no SQLSTATE")) is None
    assert unavailable_reason(RuntimeError("a bug")) is None


# ---- POST /v1/orders: nothing half-written, and a retry creates the order once -------------------

# What POST /v1/orders runs inside its transaction, in order (orders/idempotency.py, services.py).
STATEMENTS_IN_THE_TRANSACTION = [
    'INSERT INTO "orders_idempotencyrecord"',  # claim the key
    'FROM "orders_quote"',  # lock the quote
    'INSERT INTO "orders_order"',
    'INSERT INTO "orders_orderevent"',
    'INSERT INTO "messaging_outboxevent"',
    'UPDATE "orders_idempotencyrecord"',  # store the response: the last statement before COMMIT
]


@real_transactions
@pytest.mark.parametrize("statement", STATEMENTS_IN_THE_TRANSACTION)
def test_an_outage_mid_transaction_leaves_nothing_and_the_retry_creates_once(
    tenant: Tenant, statement: str
) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    with counts_one_503("/v1/orders", "connection"), database_dies_at(statement):
        response = post_order(tenant, body, key="retry-me")
    assert_database_unavailable(response)
    assert_nothing_written()

    retry = post_order(tenant, body, key="retry-me")
    assert retry.status_code == 201
    assert "Idempotent-Replayed" not in retry.headers  # nothing was stored, so this one ran
    assert_one_order(retry.json())


@real_transactions
def test_an_outage_before_the_transaction(tenant: Tenant, caplog: pytest.LogCaptureFixture) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    shutdown = "terminating connection due to administrator command"
    # Authentication is the request's first query.
    with (
        counts_one_503("/v1/orders", "shutdown"),
        database_dies_at('FROM "accounts_apikey"', sqlstate="57P01", message=shutdown),
        caplog.at_level(logging.WARNING, logger="orders.http"),
    ):
        response = post_order(tenant, body, key="retry-me")
    assert_database_unavailable(response)
    assert_nothing_written()
    logged = next(r for r in caplog.records if r.getMessage() == "database unavailable")
    assert logged.__dict__["route"] == "/v1/orders"
    assert logged.__dict__["reason"] == "shutdown"
    assert logged.__dict__["error"] == f"AdminShutdown: {shutdown}"

    assert post_order(tenant, body, key="retry-me").status_code == 201


@real_transactions
def test_a_commit_that_never_reached_the_server(tenant: Tenant) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    with counts_one_503("/v1/orders", "connection"), commit_fails(after_committing=False):
        response = post_order(tenant, body, key="retry-me")
    assert_database_unavailable(response)
    assert_nothing_written()

    retry = post_order(tenant, body, key="retry-me")
    assert retry.status_code == 201
    assert "Idempotent-Replayed" not in retry.headers
    assert_one_order(retry.json())


@real_transactions
def test_a_commit_whose_acknowledgement_was_lost_is_replayed(tenant: Tenant) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    with counts_one_503("/v1/orders", "connection"), commit_fails(after_committing=True):
        response = post_order(tenant, body, key="retry-me")
    assert_database_unavailable(response)  # the client cannot know that the order exists...
    assert Order.objects.count() == 1  # ...but it does, with its idempotency record

    retry = post_order(tenant, body, key="retry-me")
    assert retry.status_code == 201
    assert retry.headers["Idempotent-Replayed"] == "true"  # the original response, not a 2nd order
    assert_one_order(retry.json())


@real_transactions
def test_a_replay_interrupted_by_an_outage(tenant: Tenant) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    first = post_order(tenant, body, key="retry-me")
    assert first.status_code == 201
    # The key's INSERT conflicts; reading the stored response then fails.
    with (
        counts_one_503("/v1/orders", "connection"),
        database_dies_at('FROM "orders_idempotencyrecord"'),
    ):
        response = post_order(tenant, body, key="retry-me")
    assert_database_unavailable(response)

    replay = post_order(tenant, body, key="retry-me")
    assert replay.status_code == 201
    assert replay.headers["Idempotent-Replayed"] == "true"
    assert replay.json() == first.json()
    assert_one_order(first.json())


@real_transactions
def test_a_standby_refusing_writes(tenant: Tenant) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    read_only = "cannot execute INSERT in a read-only transaction"
    with (
        counts_one_503("/v1/orders", "read_only"),
        database_dies_at("INSERT INTO", sqlstate="25006", message=read_only),
    ):
        response = post_order(tenant, body, key="retry-me")
    assert_database_unavailable(response)
    assert_nothing_written()


@real_transactions
def test_retry_after_is_configurable(tenant: Tenant, settings: Any) -> None:
    settings.DB_RETRY_AFTER_SECONDS = 7
    body = {"quote_id": quote(tenant)["quote_id"]}
    with database_dies_at('INSERT INTO "orders_order"'):
        response = post_order(tenant, body)
    assert_database_unavailable(response, retry_after="7")


@real_transactions
def test_a_deadlock_is_still_a_500(tenant: Tenant) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    tenant.client.raise_request_exception = False
    with database_dies_at('INSERT INTO "messaging_outboxevent"', sqlstate="40P01"):
        response = post_order(tenant, body)
    assert response.status_code == 500
    assert response.json()["error"]["code"] == "internal_error"
    assert "Retry-After" not in response.headers
    assert_nothing_written()


@pytest.mark.postgres
@real_transactions
@pytest.mark.parametrize(
    ("failure", "reason"),
    [
        # A switchover or a deleted primary pod: the server ends the session (57P01).
        (["SELECT pg_terminate_backend(pg_backend_pid())"], "shutdown"),
        # A statement that outlives the statement timeout (57014).
        (["SET LOCAL statement_timeout = '10ms'", "SELECT pg_sleep(1)"], "timeout"),
        # A standby refuses writes (25006).
        (["SET TRANSACTION READ ONLY"], "read_only"),
    ],
    ids=["session-terminated", "statement-timeout", "read-only"],
)
def test_a_real_server_failure_mid_transaction(
    tenant: Tenant, failure: list[str], reason: str
) -> None:
    body = {"quote_id": quote(tenant)["quote_id"]}
    # By the outbox INSERT, the order and its history are already written in the transaction.
    with (
        counts_one_503("/v1/orders", reason),
        server_fails_before('INSERT INTO "messaging_outboxevent"', failure),
    ):
        response = post_order(tenant, body, key="retry-me")
    assert_database_unavailable(response)
    assert_nothing_written()

    retry = post_order(tenant, body, key="retry-me")  # on a new connection after a termination
    assert retry.status_code == 201
    assert "Idempotent-Replayed" not in retry.headers
    assert_one_order(retry.json())


# ---- other routes --------------------------------------------------------------------------------


@real_transactions
def test_quotes_answer_503_too(tenant: Tenant) -> None:
    with counts_one_503("/v1/quotes", "connection"), database_dies_at('INSERT INTO "orders_quote"'):
        response = tenant.client.post(
            "/v1/quotes", {"store_id": tenant.store.id, "dropoff": dropoff()}, format="json"
        )
    assert_database_unavailable(response)
    assert not Quote.objects.exists()


def test_every_operation_documents_the_503() -> None:
    schema = SchemaGenerator().get_schema(request=None, public=True)
    operations = [op for path_item in schema["paths"].values() for op in path_item.values()]
    assert operations
    for operation in operations:
        assert "Retry-After" in operation["responses"]["503"]["headers"]


# ---- readiness: a short outage keeps the pod ready -----------------------------------------------


class FakeClock:
    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now


class DownConnection:
    """Stands in for django.db.connection (in core.middleware) while connections are refused."""

    def cursor(self) -> Any:
        raise django_error(None, "connection refused")


def test_readiness_waits_for_the_first_answer() -> None:
    # A new pod with a wrong DATABASE_URL takes no traffic, however long it waits.
    clock = FakeClock()
    readiness = DatabaseReadiness(grace_s=30, clock=clock)
    assert not readiness.record(answered=False).ready
    clock.now += 600
    assert not readiness.record(answered=False).ready
    assert readiness.record(answered=True).ready


def test_readiness_rides_out_failures_shorter_than_the_grace() -> None:
    clock = FakeClock()
    readiness = DatabaseReadiness(grace_s=30, clock=clock)
    assert readiness.record(answered=True) == ReadinessVerdict(ready=True, failing_for_s=0.0)
    assert readiness.record(answered=False) == ReadinessVerdict(ready=True, failing_for_s=0.0)
    clock.now += 29
    assert readiness.record(answered=False) == ReadinessVerdict(ready=True, failing_for_s=29.0)
    clock.now += 1
    assert readiness.record(answered=False) == ReadinessVerdict(ready=False, failing_for_s=30.0)
    # The database answers again: the streak is over, and the next outage gets a fresh grace.
    assert readiness.record(answered=True).ready
    clock.now += 100
    assert readiness.record(answered=False) == ReadinessVerdict(ready=True, failing_for_s=0.0)


def test_readiness_grace_zero_fails_at_the_first_failure() -> None:
    readiness = DatabaseReadiness(grace_s=0, clock=FakeClock())
    assert readiness.record(answered=True).ready
    assert not readiness.record(answered=False).ready


@real_transactions
def test_readyz_during_an_outage(monkeypatch: pytest.MonkeyPatch, tenant: Tenant) -> None:
    """GD-06 in miniature: within the grace the pod stays ready, and its 503s reach the SLI."""
    clock = FakeClock()
    monkeypatch.setattr(
        "core.middleware.database_readiness", DatabaseReadiness(grace_s=120, clock=clock)
    )
    probe = APIClient()
    assert probe.get("/readyz").json() == {"status": "ready"}
    body = {"quote_id": quote(tenant)["quote_id"]}

    monkeypatch.setattr("core.middleware.connection", DownConnection())  # the failover starts
    response = probe.get("/readyz")
    assert response.status_code == 200
    assert response.json() == {"status": "degraded", "database_failing_for_s": 0}
    with counts_one_503("/v1/orders", "connection"), database_dies_at("INSERT INTO"):
        assert_database_unavailable(post_order(tenant, body))
    assert probe.get("/healthz").status_code == 200  # liveness never looks at the database

    clock.now += 119
    assert probe.get("/readyz").json() == {"status": "degraded", "database_failing_for_s": 119}
    clock.now += 1  # a database still down after the grace takes the pod out of the Service
    response = probe.get("/readyz")
    assert response.status_code == 503
    assert response.json() == {"status": "database_unreachable"}

    monkeypatch.setattr("core.middleware.connection", connection)  # a new primary answers
    assert probe.get("/readyz").json() == {"status": "ready"}
