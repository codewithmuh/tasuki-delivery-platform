"""What the API does while PostgreSQL is unavailable (design §10: "Clients get 503 Retry-After").

Two decisions live here:

1. `unavailable_reason`: which database errors mean "the database cannot serve this request right
   now" (a failover, a restart, a standby, no free connection slot, a statement timeout). Those
   become 503 database_unavailable + Retry-After (core.middleware.DatabaseUnavailableMiddleware).
   Any other database error (a constraint violation, a bad query, a deadlock) is a bug or a
   conflict, and stays a 500.
2. `DatabaseReadiness`: when a failing database makes the pod unready (only once it has failed for
   longer than READINESS_DB_GRACE_SECONDS). The reasons are in the class docstring.
"""

from __future__ import annotations

import threading
import time
from collections.abc import Callable
from dataclasses import dataclass

from django.db import InterfaceError, InternalError, OperationalError

# SQLSTATEs (PostgreSQL manual, appendix A) that mean the server cannot serve us right now, with
# the `reason` label they get in orders_database_unavailable_total. Class 08 (connection
# exception) is matched by its prefix in unavailable_reason.
_UNAVAILABLE_SQLSTATES = {
    "57P01": "shutdown",  # admin_shutdown: the server is stopping (switchover, pod deleted)
    "57P02": "shutdown",  # crash_shutdown: another backend crashed and the server is restarting
    "57P03": "shutdown",  # cannot_connect_now: the server is starting up or recovering
    "25006": "read_only",  # read_only_sql_transaction: connected to a standby, not the primary
    "53300": "too_many_connections",  # every connection slot is taken
    "57014": "timeout",  # query_canceled: DB_STATEMENT_TIMEOUT_MS ran out
}


def unavailable_reason(exc: BaseException) -> str | None:
    """Why the database cannot serve this request right now, or None if `exc` means something else.

    psycopg reports both outages and some bugs as OperationalError (a deadlock is one), so the
    exception class is not enough: the SQLSTATE the server sent decides.
    """
    if isinstance(exc, InterfaceError):
        return "connection"  # the connection is already closed
    if not isinstance(exc, OperationalError | InternalError):
        return None  # IntegrityError, ProgrammingError, DataError, ...: a bug or bad data
    # Django re-raises psycopg's error as its own class, with psycopg's error as the cause.
    sqlstate = getattr(exc.__cause__, "sqlstate", None)
    if sqlstate is None:
        # No SQLSTATE: the server never answered (connection refused, reset or timed out). SQLite,
        # in tests, has no SQLSTATEs at all, so all of its OperationalErrors land here too.
        return "connection" if isinstance(exc, OperationalError) else None
    if sqlstate.startswith("08"):
        return "connection"
    return _UNAVAILABLE_SQLSTATES.get(sqlstate)


@dataclass(frozen=True)
class ReadinessVerdict:
    ready: bool
    failing_for_s: float  # how long the database has been failing (0 when it answered)


class DatabaseReadiness:
    """Readiness and the database: a pod goes unready only after the database failed for `grace_s`.

    Every orders pod shares one PostgreSQL. If /readyz failed at the first failed check, a failover
    would take every pod out of the Service within about 10 s. Clients would then get connection
    errors (no 503, no Retry-After), and the availability SLI would record nothing, because the
    requests would never reach Django (docs/gameday/experiments/06-postgres-failover.md).

    So for `grace_s` after the first failed check the pod stays ready: requests reach Django and get
    503 database_unavailable + Retry-After, which clients retry and the SLI counts. A database that
    is still failing after that, or a pod that alone cannot reach it, takes the pod out.

    A process that has never reached the database is not ready at all, so a new pod with a wrong
    DATABASE_URL takes no traffic during a rollout.

    The state is per process (each gunicorn worker keeps its own); the lock is for its threads.
    `clock` is injectable so tests can move time.
    """

    def __init__(self, grace_s: float, clock: Callable[[], float] = time.monotonic) -> None:
        self.grace_s = grace_s
        self._clock = clock
        self._lock = threading.Lock()
        self._ever_answered = False
        self._failing_since: float | None = None

    def record(self, answered: bool) -> ReadinessVerdict:
        """Record one database check (did the database answer?) and decide readiness."""
        with self._lock:
            if answered:
                self._ever_answered = True
                self._failing_since = None
                return ReadinessVerdict(ready=True, failing_for_s=0.0)
            now = self._clock()
            if self._failing_since is None:
                self._failing_since = now  # the first failure of this streak
            failing_for = now - self._failing_since
            ready = self._ever_answered and failing_for < self.grace_s
            return ReadinessVerdict(ready=ready, failing_for_s=failing_for)
