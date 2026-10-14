# Contracts

The agreements between Tasuki's services: event schemas, HTTP endpoints, Redis keys and shared reference data.
Services may add fields; they may not rename or remove one without a new schema version (`.v2`).

Design background: [docs/design.md](../docs/design.md) §6–8.

## Events (Kafka)

All events are JSON (UTF-8), one event per message. Every event has `schema`, `event_id` (ULID) and an RFC 3339
timestamp. Delivery is **at-least-once**: consumers must dedupe on `event_id` or be naturally idempotent.

| Topic | Key | Schema | Producer | Consumers |
|---|---|---|---|---|
| `courier.locations` | `courier_id` | [courier-location.v1](events/courier-location.v1.schema.json) | fleet-ingest | fleet-state, tracking |
| `orders.events` | `order_id` | [order-event.v1](events/order-event.v1.schema.json) | orders (outbox relay) | dispatch, tracking |
| `dispatch.events` | `order_id` | [dispatch-event.v1](events/dispatch-event.v1.schema.json) | dispatch | orders, tracking |

Partitions: 12 per topic. Retention: `courier.locations` 24 h, the others 7 days.

## HTTP APIs (P1)

All APIs are JSON over HTTPS, versioned under `/v1`, and return errors as
`{"error": {"code": "snake_case", "message": "..."}}`. Every service also serves `/healthz`, `/readyz` and `/metrics`
(see the runtime contract in [docs/architecture.md](../docs/architecture.md)).

| Service | Method and path | Caller | Purpose |
|---|---|---|---|
| orders | `POST /v1/quotes` | merchant | Price and ETA for a delivery; valid 2 minutes |
| orders | `POST /v1/orders` (`Idempotency-Key` header required) | merchant | Create an order from a quote; returns `tracking_url`, the customer page `<TRACKING_PAGE_URL>/track/<token>` (the page streams from tracking) |
| orders | `GET /v1/orders/{id}` | merchant | Order with status history |
| orders | `POST /v1/orders/{id}/cancel` | merchant | Cancel before pickup |
| orders | `POST /v1/courier/orders/{id}/pickup` | courier | Courier has the order |
| orders | `POST /v1/courier/orders/{id}/deliver` | courier | Delivered (proof: lat/lng within 500 m of the drop-off) |
| orders | `POST /v1/courier/orders/{id}/fail` | courier | Delivery failed after pickup (e.g. customer unreachable) |
| orders | `POST /v1/admin/merchants`, `POST /v1/admin/couriers` | seeding/admin | Create tenants and couriers; returns API key / courier token |
| fleet-ingest | `POST /v1/locations` | courier | A batch of 1–50 GPS points for the calling courier |
| dispatch | `GET /v1/eta?ward=&store_lat=&store_lng=&dropoff_lat=&dropoff_lng=` | orders | ETA and courier supply for a quote |
| dispatch | `GET /v1/couriers/me/offers/stream` | courier | Server-sent events: offers for this courier |
| dispatch | `POST /v1/offers/{offer_id}/accept`, `.../decline` | courier | Answer an offer before it expires (30 s) |
| tracking | `GET /v1/track/{token}/stream` | customer, merchant | Server-sent events: the assigned courier's position and order status |
| tracking | `GET /v1/fleet/stream?ward=` | ops console | Server-sent events: every courier in a ward (or all) |

### Response shapes (P1)

Fields may be added later; these ones are guaranteed. Times are RFC 3339 UTC; money is integer yen.

| Call | Success response |
|---|---|
| `POST /v1/admin/merchants` `{"name", "stores": [{"name", "lat", "lng", "ward"}]}` | `201 {"merchant_id", "api_key", "stores": [{"store_id", "name", "lat", "lng", "ward"}]}` |
| `POST /v1/admin/couriers` `{"name", "vehicle": "bicycle"\|"scooter"}` | `201 {"courier_id", "token"}` |
| `POST /v1/quotes` `{"store_id", "dropoff": {"lat", "lng", "ward", "address", "customer_name", "customer_phone"}}` | `201 {"quote_id", "fee_yen", "fee_excl_tax_yen", "tax_yen", "eta_seconds", "promised_at", "expires_at", "eta_source": "dispatch"\|"fallback"}` |
| `POST /v1/orders` `{"quote_id"}` | `201 {"order_id", "status": "confirmed", "promised_at", "fee_yen", "tracking_url", "created_at"}` |
| `POST /v1/courier/orders/{id}/pickup` / `deliver` (`deliver` body: `{"lat", "lng"}`) | `200 {"order_id", "status"}` |
| `GET /v1/eta?…` (dispatch) | `200 {"eta_seconds", "pickup_s", "travel_s", "supply": "ok"\|"low"\|"none"}` |
| `POST /v1/locations` `{"points": [{"lat", "lng", "accuracy_m", "speed_mps", "heading_deg", "device_time", "status", "order_id"}]}` | `202 {"accepted": n, "dropped": n}` · `503` + `Retry-After` when shedding load |
| `POST /v1/offers/{offer_id}/accept` / `decline` | `200 {"offer_id", "order_id", "status": "accepted"\|"declined"}` · `409` if expired or taken |

Details the table leaves out:

- **Admin calls** use `Authorization: Bearer <ADMIN_TOKEN>`. `POST /v1/admin/couriers/{courier_id}/token` (no body)
  re-issues a courier token: `200 {"courier_id", "token"}`. Courier tokens last 24 h (`COURIER_TOKEN_TTL_SECONDS`), and
  API keys are shown only once, at creation.
- **Orders errors:** `404 quote_not_found`, `409 quote_expired`, `409 quote_already_used`,
  `422 idempotency_key_reused` (same key, different body). A replayed response carries `Idempotent-Replayed: true`.
  Drop-offs must lie inside the bbox in [data/wards.json](data/wards.json).
- **Pickup** has an empty body. **Deliver** proof must be within 500 m of the drop-off (`DELIVERY_PROOF_RADIUS_M`),
  otherwise `422 proof_too_far`. `404 not_found` or `409 illegal_transition` means the order is no longer this
  courier's.
- **Offer accept/decline** has an empty body.
- **Locations:** `order_id` is omitted (or null) when the courier is idle; `device_time` is RFC 3339 UTC. P1 does no
  speed or "teleport" plausibility checks, because the simulator's speed-up moves couriers faster than real time;
  any check added later must be switchable off for simulation.

### Server-sent event streams

Every stream sends a comment line `: ping` every 15 s. Event `data` is one JSON object. `ward` is null outside the 23 wards.

| Stream | `event:` | `data` |
|---|---|---|
| tracking `GET /v1/fleet/stream?ward=` | `courier` | `{"courier_id", "lat", "lng", "status", "order_id", "ward", "device_time", "received_at"}` (`ward` = nearest ward centre, P1 approximation) |
| | `courier_gone` | `{"courier_id"}` (not seen for 60 s) |
| tracking `GET /v1/track/{token}/stream` | `status` | `{"order_id", "status": "confirmed"\|"assigned"\|"picked_up"\|"delivered"\|"cancelled"\|"failed", "eta", "updated_at"}` (`eta` = RFC 3339 time or null) |
| | `position` | `{"courier_id", "lat", "lng", "received_at"}` |
| | `end` | `{"reason": "delivered"\|"cancelled"\|"failed"\|"expired"}`, then the server closes the stream |
| dispatch `GET /v1/couriers/me/offers/stream` | `offer` | `{"offer_id", "order_id", "store": {"id", "lat", "lng"}, "dropoff": {"lat", "lng"}, "ready_at", "expires_at", "eta_pickup_s", "eta_dropoff_s"}` (`ready_at` may be null) |
| | `offer_cancelled` | `{"offer_id", "reason"}` |

A tracking stream for an unknown order returns `404`; an invalid or expired token returns `401`. On connect, tracking first sends the current
state (every courier in scope on the fleet stream; the current `status` and last `position` on a track stream), so a
page fills at once. When a courier changes ward, viewers of the old ward get one `courier_gone` for it. Streams send
`X-Accel-Buffering: no` so proxies don't buffer them, and tracking tokens are never logged. Clients ignore
event names they don't know, and reconnect if a stream is silent for 45 s (three missed pings).

**Exception to the runtime contract:** OSRM (`routing`) has no `/healthz` or `/readyz`; probes call
`GET /nearest/v1/bicycle/139.7016,35.6580` (Shibuya) instead.

### Error codes

| Endpoint | Codes |
|---|---|
| orders (every endpoint) | `503 database_unavailable` + `Retry-After` while PostgreSQL is unreachable, failing over or read-only; safe to retry (idempotency keys) |
| `POST /v1/locations` | `400 invalid_json`, `401 unauthorized` / `invalid_token`, `413 body_too_large`, `422 invalid_batch` / `invalid_points`, `503 overloaded` (+ `Retry-After`) |
| tracking streams | `400 unknown_ward`, `403 fleet_stream_disabled`, `503 too_many_viewers` |
| dispatch | `400 invalid_query` / `unknown_ward`, `401 unauthorized` / `invalid_token`, `404 offer_not_found`, `409 offer_not_yours` / `offer_expired` / `offer_closed` / `courier_busy`, `503 too_many_streams` / `unavailable` |

`offer_cancelled.reason` is one of `order_cancelled`, `order_closed`, `expired`.

### Authentication (P1)

- **Merchants:** `Authorization: Bearer <api_key>`. Keys are random, stored hashed in the orders DB. (OAuth2 client
  credentials replace this in P3.)
- **Couriers:** `Authorization: Bearer <JWT>`, HS256, signed with `COURIER_TOKEN_SECRET` (shared by orders, which
  issues them, and the Go services, which verify them). Claims: `sub` = courier ID, `role` = `courier`, `exp`.
- **Tracking links:** `/v1/track/{token}` where `token` is a JWT (HS256, `TRACKING_TOKEN_SECRET`) with `sub` = order ID,
  `role` = `tracking`, and `exp` = 24 h after the promised delivery time.
- Secrets come from environment variables, never from code. Local defaults live in `.env.example`.

## Redis keys (fleet-state writes, dispatch and tracking read)

| Key | Type | Content |
|---|---|---|
| `courier:{courier_id}` | hash | `lat`, `lng`, `status`, `order_id`, `device_time`, `device_time_ms`, `received_at`; TTL 10 min |
| `geo:couriers:available` | geo set | Couriers whose last status is `idle` and who were seen in the last 60 s |
| `geo:couriers:all` | geo set | Every courier seen in the last 60 s |
| `couriers:last_seen` | sorted set | courier ID → `received_at` in ms (drives the 60 s sweeper) |
| `fleet:state:rebuilt_at` | string | When fleet-state last finished rebuilding Redis from Kafka; dispatch waits for it after a Redis loss |

Redis is a cache derived from `courier.locations`: it can always be rebuilt by replaying the topic. Redis runs as a
primary with replicas, not in cluster mode, because the Lua scripts touch several keys at once.
P1 uses Redis GEO commands for "nearest couriers"; H3 cells arrive with batched dispatch in P3 (ADR 0008).

## Reference data

- [data/wards.json](data/wards.json): the 23 special wards of Tokyo with approximate centre coordinates (synthetic use
  only). Ward IDs (`shibuya`, `minato`, …) are the dispatch zone IDs everywhere.

## IDs

ULIDs with a type prefix: `ord_01J…`, `mer_…`, `str_…`, `cou_…`, `off_…`, `evt_…`. Sortable by time and not guessable.
