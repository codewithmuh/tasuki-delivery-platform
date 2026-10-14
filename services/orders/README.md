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
Errors are always `{"error": {"code": "snake_case", "message": "..."}}`.

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
| `GET /healthz`, `/readyz`, `/metrics` | none | Answered before the Host check, so probes and scrapes by pod IP work |

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
(the quote row is locked and `Order.quote` is unique).

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
| `TRACKING_BASE_URL` | `http://127.0.0.1:8083` | `tracking_url` = this + `/v1/track/<JWT>` |
| `KAFKA_BROKERS` | `localhost:9092` | Relay and consumer only |
| `KAFKA_ORDERS_TOPIC`, `KAFKA_DISPATCH_TOPIC`, `KAFKA_CONSUMER_GROUP` | `orders.events`, `dispatch.events`, `orders` | |
| `COURIER_TOKEN_TTL_SECONDS` | `86400` | |
| `QUOTE_TTL_SECONDS`, `IDEMPOTENCY_TTL_HOURS` | `120`, `24` | |
| `NO_COURIER_CANCEL_AFTER_SECONDS` | `900` | |
| `DELIVERY_PROOF_RADIUS_M` | `500` | |
| `WEB_CONCURRENCY`, `GUNICORN_THREADS`, `PORT` | `2`, `4`, `8000` | |
| `WORKER_METRICS_PORT` | `9100` | Relay and consumer: `/healthz` (loop heartbeat), `/readyz`, `/metrics` |
| `DB_CONN_MAX_AGE`, `DB_STATEMENT_TIMEOUT_MS` | `60`, `5000` | |
| `LOG_LEVEL`, `DJANGO_DEBUG` | `INFO`, `false` | |

## Observability

- Logs: one JSON object per line on stdout, with `request_id` (from `X-Request-ID` or generated, and echoed back).
  librdkafka's messages are routed into the same format.
- Metrics: `http_requests_total{method,route,status}` and `http_request_duration_seconds{method,route}` (route is the
  URL pattern, never the raw path); `orders_quotes_created_total{eta_source}`, `orders_created_total`,
  `orders_idempotent_replays_total`, `orders_eta_fallbacks_total{reason}`, `orders_status_transitions_total{status}`;
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
- Deleting order PII 90 days after delivery (design §11), per-tenant rate limits, OAuth2 client credentials (P3).
