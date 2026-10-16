# Load test: GD-01 local re-run after the fixes: normal-weekday traffic across a Valkey loss

| | |
|---|---|
| Started | 2026-10-16 11:49:52 UTC |
| Window | 4.0 min (240 s) |
| Scenario | normal-weekday, started at 15:00 simulated time |
| Measured by | `scripts/loadtest/report.py` (scrapes every service's `/metrics`; PromQL-equivalent maths) |

## Load the simulator generated

| What | Total | Per second | Per hour |
|---|---:|---:|---:|
| Orders attempted | 12 | 0.05 | 180 |
| Orders created | 12 | 0.05 | 180 |
| GPS points accepted | 7,092 | 29.5 | |
| GPS points dropped | 0 | 0.0 | |
| GPS batches sent | 7,092 | 29.5 | |
| Offers accepted | 10 | 0.04 | |
| Offers declined | 15 | 0.06 | |
| Offers failed | 0 | 0.00 | |
| Offers lost | 0 | 0.00 | |
| Offers received | 27 | 0.11 | |
| Deliveries completed | 10 | 0.04 | 150 |

Couriers online at the end: **121**. Intended demand at the end (scenario curve × weather): **209 orders/h**.

## SLOs

| SLO | Target | Measured | Verdict |
|---|---:|---:|---|
| Quote and order requests succeed (not 5xx) | 99.9 % | 100.000 % (0 of 24 failed) | met |
| `POST /v1/orders` under 300 ms | 95 % | 100.00 % | met |
| GPS points in live state within 2 s | 99 % | 92.16 % | **missed** |
| Viewers see positions younger than 10 s | 99 % | 100.00 % | met |
| A courier accepts within 60 s | 95 % | 38.46 % | **missed** |

## Latency

Server side (`http_request_duration_seconds`, what the SLO measures):

| Endpoint | Requests/s | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| POST /v1/quotes (orders) | 0.0 | 25 ms | 240 ms | 288 ms |
| POST /v1/orders (orders) | 0.0 | 19 ms | 42 ms | 48 ms |
| POST /v1/locations (fleet-ingest) | 29.5 | 17 ms | 24 ms | 25 ms |

Client side, as the simulator saw it (includes the network and any queueing):

| Call | Requests/s | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| `accept` | 0.0 | 5 ms | 10 ms | 10 ms |
| `decline` | 0.1 | 6 ms | 1938 ms | 2387 ms |
| `deliver` | 0.0 | 25 ms | 48 ms | 50 ms |
| `locations` | 29.5 | 18 ms | 24 ms | 33 ms |
| `offer_stream` | 0.0 | 6 ms | 75 ms | 95 ms |
| `order` | 0.0 | 19 ms | 42 ms | 48 ms |
| `pickup` | 0.1 | 23 ms | 79 ms | 96 ms |
| `quote` | 0.0 | 25 ms | 380 ms | 476 ms |
| `route` | 0.2 | 6 ms | 20 ms | 24 ms |
| `track_stream` | 0.0 | 283 ms | 825 ms | 965 ms |

## Location pipeline and dispatch

| Signal | Value |
|---|---:|
| GPS points accepted by fleet-ingest | 29.5/s |
| fleet-ingest requests shed (503) | 0 |
| Location freshness p99 | 25.6 s |
| Tracking event age p99 | 0.2 s |
| Tracking SSE connections at the end | 49 |
| Tracking events dropped (slow viewers) | 0 |
| Assignment time p50 / p95 (assigned orders) | 22.5 s / 41.2 s |
| Orders with no courier (first report) | 8 of 13 |
| Dispatch offers accepted | 10 |
| Dispatch offers declined | 13 |
| Dispatch offers expired | 0 |
| Dispatch offers sent | 27 |
| Dispatch offers withdrawn | 0 |
| Dispatch pending orders at the end | 20 |
| Dispatch offer streams open at the end | 121 |
| ETA requests from `fallback` | 36 |
| ETA requests from `osrm` | 0 |

## What viewers saw

Customers with the tracking page open, measured by the simulator (`simulator_tracking_*`).

| Signal | Value |
|---|---:|
| Tracking pages open at the end | 49 |
| `end` events received | 8 (0.0/s) |
| `position` events received | 2,061 (8.6/s) |
| `status` events received | 33 (0.1/s) |
| Streams finished: `ended` | 8 |
| Position age when received p50 / p99 | 0.1 s / 0.2 s |
| Positions younger than 10 s | 99.81 % |
| Couriers by state at the end | at_store 27, idle 3, to_dropoff 31, to_store 60 |

## Resources at the end of the window

`docker stats` (CPU is per core: 100 % = one core).

| Container | CPU | Memory |
|---|---:|---|
| tasuki-delivery-platform-console-1 | 0.00% | 9.18MiB / 7.748GiB |
| tasuki-delivery-platform-dispatch-1 | 3.31% | 56.73MiB / 7.748GiB |
| tasuki-delivery-platform-fleet-ingest-1 | 3.26% | 88.07MiB / 7.748GiB |
| tasuki-delivery-platform-fleet-state-1 | 0.99% | 38.11MiB / 7.748GiB |
| tasuki-delivery-platform-kafka-1 | 7.27% | 1.153GiB / 7.748GiB |
| tasuki-delivery-platform-orders-1 | 0.05% | 152.5MiB / 7.748GiB |
| tasuki-delivery-platform-orders-consumer-1 | 0.12% | 61.56MiB / 7.748GiB |
| tasuki-delivery-platform-orders-db-1 | 0.33% | 102MiB / 7.748GiB |
| tasuki-delivery-platform-orders-relay-1 | 0.81% | 61.55MiB / 7.748GiB |
| tasuki-delivery-platform-redis-1 | 2.34% | 21.33MiB / 7.748GiB |
| tasuki-delivery-platform-tracking-1 | 1.47% | 200.9MiB / 7.748GiB |
| tasuki-loadtest-simulator | 3.77% | 36.02MiB / 7.748GiB |

## Timeline

One line per sample: the rate and SLIs over that interval only.

```text
16:50:02  orders/h 358 · GPS pts/s 28 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:50:12  orders/h 0 · GPS pts/s 31 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:50:22  orders/h 0 · GPS pts/s 28 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:50:32  orders/h 0 · GPS pts/s 31 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:50:42  orders/h 715 · GPS pts/s 28 · POST /v1/orders p95 49 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:50:52  orders/h 717 · GPS pts/s 31 · POST /v1/orders p95 24 ms · location fresh ≤2s n/a · assigned ≤60s n/a
16:51:02  orders/h 0 · GPS pts/s 28 · POST /v1/orders p95 n/a · location fresh ≤2s 13.53 % · assigned ≤60s n/a
16:51:12  orders/h 0 · GPS pts/s 31 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:51:22  orders/h 358 · GPS pts/s 28 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s 100.00 %
16:51:32  orders/h 0 · GPS pts/s 31 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:51:42  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:51:53  orders/h 0 · GPS pts/s 31 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:52:03  orders/h 0 · GPS pts/s 28 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:52:13  orders/h 359 · GPS pts/s 31 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:52:23  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:52:33  orders/h 359 · GPS pts/s 30 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:52:43  orders/h 358 · GPS pts/s 29 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s 100.00 %
16:52:53  orders/h 359 · GPS pts/s 30 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s 100.00 %
16:53:03  orders/h 359 · GPS pts/s 29 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:53:13  orders/h 358 · GPS pts/s 30 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s 100.00 %
16:53:23  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:53:33  orders/h 0 · GPS pts/s 30 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:53:43  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:53:52  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
```
