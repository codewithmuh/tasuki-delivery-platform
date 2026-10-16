# Load test: rainy-friday at one-tenth of design scale, rain peak, one laptop

| | |
|---|---|
| Started | 2026-10-16 10:52:42 UTC |
| Window | 15.0 min (900 s) |
| Scenario | rainy-friday, starting at 11:45 simulated time |
| Measured by | `scripts/loadtest/report.py` (scrapes every service's `/metrics`; PromQL-equivalent maths) |

## Load the simulator generated

| What | Total | Per second | Per hour |
|---|---:|---:|---:|
| Orders attempted | 458 | 0.51 | 1,832 |
| Orders created | 458 | 0.51 | 1,832 |
| GPS points accepted | 49,176 | 54.6 | |
| GPS points dropped | 0 | 0.0 | |
| GPS batches sent | 49,176 | 54.6 | |
| Offers accepted | 213 | 0.24 | |
| Offers declined | 396 | 0.44 | |
| Offers received | 610 | 0.68 | |
| Deliveries completed | 141 | 0.16 | 564 |

Couriers online at the end: **230**. Intended demand at the end (scenario curve × weather): **1,800 orders/h**.

## SLOs

| SLO | Target | Measured | Verdict |
|---|---:|---:|---|
| Quote and order requests succeed (not 5xx) | 99.9 % | 100.000 % (0 of 916 failed) | met |
| `POST /v1/orders` under 300 ms | 95 % | 100.00 % | met |
| GPS points in live state within 2 s | 99 % | 100.00 % | met |
| Viewers see positions younger than 10 s | 99 % | 100.00 % | met |
| A courier accepts within 60 s | 95 % | 25.46 % | **missed** |

## Latency

Server side (`http_request_duration_seconds`, what the SLO measures):

| Endpoint | Requests/s | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| POST /v1/quotes (orders) | 0.5 | 9 ms | 24 ms | 40 ms |
| POST /v1/orders (orders) | 0.5 | 9 ms | 23 ms | 25 ms |
| POST /v1/locations (fleet-ingest) | 0.0 | n/a | n/a | n/a |

Client side, as the simulator saw it (includes the network and any queueing):

| Call | Requests/s | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| `accept` | 0.2 | 5 ms | 10 ms | 10 ms |
| `decline` | 0.4 | 5 ms | 10 ms | 10 ms |
| `deliver` | 0.2 | 9 ms | 30 ms | 46 ms |
| `locations` | 54.6 | 17 ms | 24 ms | 25 ms |
| `order` | 0.5 | 8 ms | 23 ms | 25 ms |
| `pickup` | 0.2 | 10 ms | 25 ms | 50 ms |
| `quote` | 0.5 | 10 ms | 25 ms | 45 ms |
| `route` | 0.5 | 5 ms | 10 ms | 21 ms |
| `track_stream` | 0.3 | 358 ms | 878 ms | 976 ms |

## Location pipeline and dispatch

| Signal | Value |
|---|---:|
| GPS points accepted by fleet-ingest | 54.6/s |
| fleet-ingest requests shed (503) | 0 |
| Location freshness p99 | 0.0 s |
| Tracking event age p99 | 0.2 s |
| Tracking SSE connections at the end | 288 |
| Tracking events dropped (slow viewers) | 0 |
| Assignment time p50 / p95 (assigned orders) | 7.7 s / 49.0 s |
| Orders with no courier (first report) | 323 of 436 |
| Dispatch offers accepted | 213 |
| Dispatch offers declined | 396 |
| Dispatch offers expired | 0 |
| Dispatch offers sent | 610 |
| Dispatch offers withdrawn | 0 |
| Dispatch pending orders at the end | 251 |
| Dispatch offer streams open at the end | 230 |
| ETA requests from `fallback` | 1,068 |
| ETA requests from `osrm` | 0 |

## Resources at the end of the window

`docker stats` (CPU is per core: 100 % = one core).

| Container | CPU | Memory |
|---|---:|---|
| tasuki-delivery-platform-console-1 | 0.00% | 9.18MiB / 7.748GiB |
| tasuki-delivery-platform-dispatch-1 | 3.94% | 42.74MiB / 7.748GiB |
| tasuki-delivery-platform-fleet-ingest-1 | 2.13% | 28.68MiB / 7.748GiB |
| tasuki-delivery-platform-fleet-state-1 | 1.38% | 22.63MiB / 7.748GiB |
| tasuki-delivery-platform-kafka-1 | 4.24% | 1.045GiB / 7.748GiB |
| tasuki-delivery-platform-orders-1 | 0.02% | 147.8MiB / 7.748GiB |
| tasuki-delivery-platform-orders-consumer-1 | 0.33% | 60.47MiB / 7.748GiB |
| tasuki-delivery-platform-orders-db-1 | 1.24% | 76.4MiB / 7.748GiB |
| tasuki-delivery-platform-orders-relay-1 | 1.13% | 59.73MiB / 7.748GiB |
| tasuki-delivery-platform-redis-1 | 0.74% | 14.18MiB / 7.748GiB |
| tasuki-delivery-platform-tracking-1 | 1.58% | 33.53MiB / 7.748GiB |
| tasuki-loadtest-simulator | 3.32% | 56.74MiB / 7.748GiB |

## Timeline

One line per sample: the rate and SLIs over that interval only.

```text
15:53:42  orders/h 1,499 · GPS pts/s 45 · POST /v1/orders p95 22 ms · location fresh ≤2s 100.00 % · assigned ≤60s 96.15 %
15:54:42  orders/h 1,859 · GPS pts/s 49 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 80.77 %
15:55:42  orders/h 1,919 · GPS pts/s 53 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 51.72 %
15:56:42  orders/h 1,859 · GPS pts/s 55 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 26.32 %
15:57:42  orders/h 1,679 · GPS pts/s 55 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s 21.88 %
15:58:42  orders/h 2,159 · GPS pts/s 57 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 24.00 %
15:59:43  orders/h 1,859 · GPS pts/s 57 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 3.23 %
16:00:43  orders/h 2,278 · GPS pts/s 57 · POST /v1/orders p95 21 ms · location fresh ≤2s 100.00 % · assigned ≤60s 9.38 %
16:01:43  orders/h 1,439 · GPS pts/s 56 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 5.00 %
16:02:43  orders/h 1,859 · GPS pts/s 56 · POST /v1/orders p95 22 ms · location fresh ≤2s 100.00 % · assigned ≤60s 16.67 %
16:03:43  orders/h 2,038 · GPS pts/s 56 · POST /v1/orders p95 22 ms · location fresh ≤2s 100.00 % · assigned ≤60s 25.00 %
16:04:43  orders/h 1,679 · GPS pts/s 56 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 10.71 %
16:05:43  orders/h 1,739 · GPS pts/s 56 · POST /v1/orders p95 22 ms · location fresh ≤2s 100.00 % · assigned ≤60s 10.71 %
16:06:43  orders/h 1,859 · GPS pts/s 56 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 3.57 %
16:07:42  orders/h 1,755 · GPS pts/s 56 · POST /v1/orders p95 22 ms · location fresh ≤2s 100.00 % · assigned ≤60s 18.75 %
```

## Findings

Written after the run, from the numbers above and a reading taken during it.

1. **The software held every SLO it controls.** No quote or order failed (0 of 916), `POST /v1/orders` stayed at
   23 ms p95 (the SLO allows 300 ms), every GPS point reached live state within 2 s, and tracking sent every position
   within 0.2 s (p99). Nothing was shed and nothing was dropped. On one laptop the busiest container used 4 % of a core.
2. **Dispatch speed was missed, and that is the scenario working as designed.** Demand was 1,832 orders/h against
   230 couriers. Deliveries ran at 564/h, which is almost exactly the design's capacity estimate (2.4 deliveries per
   courier-hour × 230 ≈ 552/h, design §4). From the third minute almost every courier was busy: a reading at
   15:59 showed 225 of 230 couriers with an order and 3 available to dispatch, with 179 orders waiting. Assigned
   orders were matched fast (p50 7.7 s); the rest waited for a courier, and 323 of 436 reported "no courier".
   This is design §4's conclusion measured: in a surge the limit is couriers, not servers.
3. **A third of the busy couriers were waiting at the store.** The same reading showed 80 couriers `at_store`,
   73 `to_store` and 72 `to_dropoff`. P1 dispatch offers an order as soon as it is confirmed, but the food takes
   5–15 minutes, so couriers ride over and wait. Offering the order so the courier arrives when the food is ready
   (the offer already carries `ready_at`) would free a large share of capacity in exactly the hours that need it.
   Recorded as an input to batched dispatch (roadmap phase 6).
4. **Opening a tracking stream took 358 ms p50 (878 ms p95), by design.** The simulated customer opens the tracking
   link the instant the order is created, before the `OrderConfirmed` event has travelled from the orders database
   through the outbox relay and Kafka to tracking; tracking waits for it (`waitForOrder`, 100 ms polls). So this row
   measures event propagation from orders to tracking under load: about a third of a second.
5. **Gaps in this report, fixed for the next run.** The fleet-ingest server-side latency row is empty because the
   reporter matched the route label in orders' format only (`/v1/orders`) and not the Go services' format
   (`POST /v1/locations`); the client-side `locations` row (24 ms p95) covers the same calls. The section on what
   viewers saw was added to the reporter after this run.
6. **No OSRM in this run.** Every ETA used the straight-line fallback (1,068 of 1,068), as the stack ran without the
   `routing` profile.
