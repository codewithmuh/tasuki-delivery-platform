# orders

The order path of Tasuki (see [docs/design.md](../../docs/design.md) §5, §7.1, §8): merchants (tenants), API keys,
stores, couriers, quotes, orders, the order state machine, idempotent order creation and the transactional outbox
to Kafka. Django 5.2 LTS + Django REST framework on PostgreSQL, Python 3.13.

One image runs four processes:

| Process | Command | What it does |
|---|---|---|
| API | `gunicorn --config python:config.gunicorn config.wsgi` (image default) | HTTP API on :8000 |
| Outbox relay | `python manage.py relay_outbox` | Publishes outbox rows to `orders.events` |
| Dispatch consumer | `python manage.py consume_dispatch_events` | Applies `dispatch.events` (assignment, no courier) |
| Cleanup | `python manage.py purge_expired` (hourly) | Deletes idempotency records, old quotes, inbox IDs, published outbox rows |

Migrations: `python manage.py migrate --noinput`, as a pre-deploy job (not at startup: replicas would race).

## API

Contract: [openapi.yaml](openapi.yaml), generated from the code (`make orders-openapi`; a test fails if it is stale)
and also served at `/v1/openapi.yaml`. Shapes agreed with other services: [contracts/README.md](../../contracts/README.md).
Errors are always `{"error": {"code": "snake_case", "message": "..."}}`. Any endpoint can answer
`503 database_unavailable` with `Retry-After` while PostgreSQL is failing over (see [Database outages](#database-outages)).

| Method and path | Auth | Notes |
|---|---|---|
| `POST /v1/quotes` | merchant key | Fee and ETA, valid 2 minutes |
| `POST /v1/orders` | merchant key + `Idempotency-Key` | `{"quote_id"}`; optional `dropoff` must match the quote |
| `GET /v1/orders/{id}` | merchant key | With status history |
| `POST /v1/orders/{id}/cancel` | merchant key | Before pickup; `reason` is a code, never free text |
| `POST /v1/courier/orders/{id}/pickup` | courier JWT | Assigned courier only (others get 404) |
| `POST /v1/courier/orders/{id}/deliver` | courier JWT | `{"lat","lng"}` within `DELIVERY_PROOF_RADIUS_M` of the drop-off |
| `POST /v1/courier/orders/{id}/fail` | courier JWT | After pickup, e.g. `customer_unreachable` |
| `POST /v1/admin/merchants` | `ADMIN_TOKEN` | Merchant + stores + API key (shown once) |
| `POST /v1/admin/couriers` | `ADMIN_TOKEN` | Courier + JWT |
| `POST /v1/admin/couriers/{id}/token` | `ADMIN_TOKEN` | A fresh JWT for an existing courier |
| `GET /healthz`, `/readyz`, `/metrics` | none | Answered before the Host check, so probes and scrapes by pod IP work. `/readyz` rides out short database outages (see [Database outages](#database-outages)) |

**Authentication.** Merchant API keys look like `tsk_<lookup>_<secret>`; only a SHA-256 of the whole key is stored
(256 random bits need no slow hash). Courier tokens and tracking links are HS256 JWTs shared with the Go services;
verification accepts HS256 only. Unset `ADMIN_TOKEN` switches the admin API off.

## How it works

**Pricing** ([pricing.py](src/orders/pricing.py)): ¥300 + ¥75 per started 500 m of straight-line distance, plus 10 %
consumption tax rounded down once per order. Integer yen throughout; the database checks
`fee_yen = fee_excl_tax_yen + tax_yen`.

**ETA** ([eta.py](src/orders/eta.py)): `GET {DISPATCH_URL}/v1/eta` with a 300 ms timeout. Dispatch's answer is never
earlier than the store's prep time plus the ride. On a timeout, an error or a bad answer, the quote uses the
straight-line distance × 1.35 at 15 km/h plus prep time, and `orders_eta_fallbacks_total{reason}` counts it.

**Idempotency** ([idempotency.py](src/orders/idempotency.py)): the key is claimed with an `INSERT` at the start of the
transaction that creates the order, and the response is stored at its end, both under a unique `(merchant, key)`.
A concurrent duplicate blocks on that index, then replays the stored response; a different body with the same key
gets `422 idempotency_key_reused`. Failed attempts store nothing. Records are kept 24 h. A quote can be ordered once
(the quote row is locked and `Order.quote` is unique). Retries after a database failure are covered in
[Database outages](#database-outages).

**State machine** ([state_machine.py](src/orders/state_machine.py)): one transition table (design §8). Every change
locks the order row, is checked against the table (`409 illegal_transition` otherwise), appends an `OrderEvent`
history row and, for published statuses, writes an outbox row, all in one transaction.

**Outbox relay** ([relay.py](src/messaging/relay.py)): takes a batch of pending rows with `FOR UPDATE SKIP LOCKED`,
publishes them to `orders.events` keyed by order ID, waits for Kafka's acknowledgements, then marks them published.
At-least-once (consumers dedupe on `event_id`). Safe with several replicas, and a relay only takes the oldest pending
event of each order, so replicas never reorder one order's events. Publishing `OrderConfirmed` moves the order to
`dispatching`. `orders_outbox_lag_seconds` is the age of the oldest pending row.

**Dispatch consumer** ([consumer.py](src/messaging/consumer.py), [dispatch_events.py](src/orders/dispatch_events.py)):
each event's ID is inserted into `ProcessedEvent` in the same transaction as its effects, and the Kafka offset is
committed after that transaction. `CourierAssigned` assigns the courier. `NoCourierAvailable` leaves the order
`dispatching` until it is `NO_COURIER_CANCEL_AFTER_SECONDS` old (15 min), then cancels it with reason
`no_courier_available`. That publishes `OrderCancelled`, so dispatch stops too.

**Personal data**: customer name, phone and address text are encrypted per field (Fernet, `ORDERS_FIELD_KEY`; AWS KMS
envelope encryption in production). They never appear in events (coordinates only), logs, API responses or
idempotency records, and tests check all four. Quotes, the only other copy, are deleted after 24 h.

## Database outages

Design §10 promises that when the PostgreSQL primary fails, "clients get `503 Retry-After`; idempotency keys make their
retries safe". Planning game day [GD-06](../../docs/gameday/experiments/06-postgres-failover.md) showed (by reading
the code; the drill has not been run yet) that orders answered `500` instead, and that its readiness hid the outage
from the SLI. Tests: [test_database_outage.py](tests/test_database_outage.py).

**503 instead of 500** ([core/database.py](src/core/database.py) decides, `DatabaseUnavailableMiddleware` in
[core/middleware.py](src/core/middleware.py) answers). When a view fails because the database cannot serve it, the
answer is `503 {"error": {"code": "database_unavailable", ...}}` with `Retry-After: DB_RETRY_AFTER_SECONDS`. The
SQLSTATE decides, not the exception class, because psycopg reports a deadlock as an `OperationalError` too:

| `503`, with the `reason` label | Still `500` |
|---|---|
| No SQLSTATE (refused, reset, timed out), class `08`, or a closed connection: `connection` | Integrity, programming and data errors: bugs or bad input |
| `57P01` admin shutdown, `57P02` crash shutdown, `57P03` starting up or recovering: `shutdown` | `40P01` deadlock, `40001` serialization failure: a locking bug, not an outage |
| `25006` read-only transaction, i.e. connected to a standby: `read_only` | Every other SQLSTATE |
| `53300` too many connections: `too_many_connections` | |
| `57014` statement cancelled by `DB_STATEMENT_TIMEOUT_MS`: `timeout` | |

- **Nothing half-written.** Every write of more than one statement runs in one `transaction.atomic()` block, which
  rolls back as the exception leaves it; the middleware answers only after that. A quote is a single `INSERT`, so a
  retried quote can at most leave an unused quote behind (it expires in 2 minutes and is purged after 24 h).
- **A retried order is created once.** The idempotency record and its stored response are written in the order's own
  transaction. A failure before or during the transaction stores nothing, so the retry runs it. If the connection
  breaks *during COMMIT*, nobody can tell whether it committed: if it did, the retry finds the record and replays the
  original `201` (`Idempotent-Replayed: true`); if not, the retry creates the order. One order either way.
  `orders_created_total` misses an order whose acknowledgement was lost; its replay counts in
  `orders_idempotent_replays_total`.
- **Counted where the SLO looks.** The 503 goes back out through the request middleware, so it is in
  `http_requests_total{status="503"}` (the availability SLI) and in `orders_database_unavailable_total{route,reason}`,
  with one warning log line that names the driver's error.
- **Bounded waits.** `DB_CONNECT_TIMEOUT_SECONDS` bounds a connection attempt. psycopg's default is 130 s, which would
  hold a worker thread instead of answering 503 when the database's address stops answering.
- **Workers unchanged.** The outbox relay and the dispatch consumer already survive outages: each batch is one
  transaction, and on an error they log, reconnect and back off. Outbox rows stay pending until the database is back.

**Readiness rides out short outages** (`DatabaseReadiness` in [core/database.py](src/core/database.py)). Every pod
shares one database. When `/readyz` failed at the first failed query, a failover emptied the Service within about 10 s:
clients got connection errors without `Retry-After`, and `http_requests_total` recorded nothing, because requests never
reached Django. Now:

- A pod whose database fails stays ready for `READINESS_DB_GRACE_SECONDS` after the first failed check, and `/readyz`
  answers `200 {"status": "degraded", "database_failing_for_s": n}`. Requests get `503` + `Retry-After`, which clients
  retry and the SLI counts.
- After the grace, `/readyz` answers `503 {"status": "database_unreachable"}` and the pod leaves the Service. The first
  check that succeeds makes it ready again. A database that stays down, or a pod that alone cannot reach it, is still
  taken out.
- A process that has never reached the database is not ready at all, so a new pod with a wrong `DATABASE_URL` takes no
  traffic during a rollout.
- Liveness (`/healthz`) never looks at the database.

Why 120 s and not 30 s: design §10 allows 1–2 minutes for a failover, and `OrdersAvailabilityBudgetBurn` pages only
when more than 1.344 % of an hour's requests fail (13.44× burn on 99.9 %), about 48 s of a full outage. With a 30 s
grace the pods would go unready, and the SLI silent, before a dead database could page. `READINESS_DB_GRACE_SECONDS=0`
brings back the old behaviour.

Limits:

- The grace clock is kept per gunicorn worker, so a pod goes unready between the grace and the grace plus a few probe
  periods.
- If the database host drops packets instead of refusing connections, the check waits up to
  `DB_CONNECT_TIMEOUT_SECONDS`. The readiness probe's `timeoutSeconds` must be longer than that (the chart's 2 s is
  not), or the kubelet counts a timeout whatever the grace.
- Status changes other than creation (cancel, pickup, ...) have no idempotency key. If such a commit went through but
  its acknowledgement was lost, the retry gets `409 illegal_transition`, and `GET /v1/orders/{id}` shows the change.
- The alternatives to the grace window are open and need an ADR: readiness that ignores the database plus a
  load-balancer SLI, or a client-side SLI.

## Configuration

Environment variables only. The process refuses to start without the required ones and lists every missing name.

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_URL` | required | `postgres://user:pass@host:5432/db` |
| `DJANGO_SECRET_KEY` | required | Also keys the drop-off hash |
| `ORDERS_FIELD_KEY` | required | Fernet key(s), comma-separated; the first encrypts, all decrypt (rotation) |
| `COURIER_TOKEN_SECRET` | required | ≥ 32 chars; the same value in the Go services |
| `TRACKING_TOKEN_SECRET` | required | ≥ 32 chars; the same value in tracking |
| `ADMIN_TOKEN` | unset (admin API off) | Seeding only |
| `DJANGO_ALLOWED_HOSTS` | `localhost,127.0.0.1,[::1]` | Add the service DNS name (e.g. `orders`) and the public host |
| `DISPATCH_URL` | `http://127.0.0.1:8082` | |
| `DISPATCH_TIMEOUT_MS` | `300` | Per phase (connect, read, ...) |
| `TRACKING_PAGE_URL` | `http://localhost:8088` | Customer tracking page (the console); `tracking_url` = this + `/track/<JWT>` |
| `KAFKA_BROKERS` | `localhost:9092` | Relay and consumer only |
| `KAFKA_ORDERS_TOPIC`, `KAFKA_DISPATCH_TOPIC`, `KAFKA_CONSUMER_GROUP` | `orders.events`, `dispatch.events`, `orders` | |
| `COURIER_TOKEN_TTL_SECONDS` | `86400` | |
| `QUOTE_TTL_SECONDS`, `IDEMPOTENCY_TTL_HOURS` | `120`, `24` | |
| `NO_COURIER_CANCEL_AFTER_SECONDS` | `900` | |
| `DELIVERY_PROOF_RADIUS_M` | `500` | |
| `WEB_CONCURRENCY`, `GUNICORN_THREADS`, `PORT` | `2`, `4`, `8000` | |
| `WORKER_METRICS_PORT` | `9100` | Relay and consumer: `/healthz` (loop heartbeat), `/readyz`, `/metrics` |
| `DB_CONN_MAX_AGE`, `DB_STATEMENT_TIMEOUT_MS` | `60`, `5000` | |
| `DB_CONNECT_TIMEOUT_SECONDS` | `3` | libpq `connect_timeout` (2 at least) |
| `DB_RETRY_AFTER_SECONDS` | `2` | `Retry-After` on `503 database_unavailable` |
| `READINESS_DB_GRACE_SECONDS` | `120` | How long a failing database keeps `/readyz` ready; `0` fails it at once |
| `LOG_LEVEL`, `DJANGO_DEBUG` | `INFO`, `false` | |

## Observability

- Logs: one JSON object per line on stdout, with `request_id` (from `X-Request-ID` or generated, and echoed back).
  librdkafka's messages are routed into the same format.
- Metrics: `http_requests_total{method,route,status}` and `http_request_duration_seconds{method,route}` (route is the
  URL pattern, never the raw path); `orders_quotes_created_total{eta_source}`, `orders_created_total`,
  `orders_idempotent_replays_total`, `orders_eta_fallbacks_total{reason}`, `orders_status_transitions_total{status}`,
  `orders_database_unavailable_total{route,reason}`;
  relay `orders_outbox_lag_seconds`, `orders_outbox_pending_events`, `orders_outbox_published_total`,
  `orders_outbox_publish_failures_total`; consumer `orders_dispatch_events_total{type,outcome}`.
- Shutdown: on SIGTERM gunicorn fails `/readyz`, drains in-flight requests for up to 20 s and exits 0. The relay and
  consumer finish their current batch, close Kafka cleanly and exit 0.

## Development

```bash
cd services/orders
uv sync                                   # Python 3.13 and the dev tools
make -C ../.. orders-lint orders-test     # what CI runs (from the repo root: make orders-lint orders-test)
```

Tests run on in-memory SQLite by default. Tests marked `postgres` need PostgreSQL semantics (blocking unique inserts,
row locks, `SKIP LOCKED`) and are skipped there. Point `DATABASE_URL` at PostgreSQL to run them all, as CI does
(`postgres:17`, with `REQUIRE_POSTGRES=1` so a misconfigured database fails instead of skipping):

```bash
DATABASE_URL=postgres://postgres@127.0.0.1:5432/orders uv run pytest
```

To run the API locally against PostgreSQL:

```bash
export DATABASE_URL=postgres://... DJANGO_SECRET_KEY=... ORDERS_FIELD_KEY=$(uv run python -c \
  'from cryptography.fernet import Fernet; print(Fernet.generate_key().decode())') \
  COURIER_TOKEN_SECRET=... TRACKING_TOKEN_SECRET=... ADMIN_TOKEN=...
uv run python manage.py migrate
PYTHONPATH=src uv run gunicorn --config python:config.gunicorn config.wsgi
```

## Not built yet

- A dead-letter topic for unreadable `dispatch.events` messages (they are logged and skipped today).
- A sweeper for orders stuck in `dispatching` if dispatch stops reporting `NoCourierAvailable`.
- `LISTEN/NOTIFY` to wake the relay at once instead of polling every 0.5 s.
- A bound on established connections to a database host that vanished (libpq `tcp_user_timeout` or keepalives): after
  a node loss, a request on such a connection can wait for TCP to give up instead of answering 503.
- Deleting order PII 90 days after delivery (design §11), per-tenant rate limits, OAuth2 client credentials (P3).
