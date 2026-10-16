# Load test: GD-01 local: normal-weekday traffic across a Valkey loss

| | |
|---|---|
| Started | 2026-10-16 11:36:59 UTC |
| Window | 9.0 min (540 s) |
| Scenario | normal-weekday, starting at 15:00 simulated time |
| Measured by | `scripts/loadtest/report.py` (scrapes every service's `/metrics`; PromQL-equivalent maths) |

## Load the simulator generated

| What | Total | Per second | Per hour |
|---|---:|---:|---:|
| Orders attempted | 39 | 0.07 | 260 |
| Orders created | 39 | 0.07 | 260 |
| GPS points accepted | 15,936 | 29.5 | |
| GPS points dropped | 0 | 0.0 | |
| GPS batches sent | 15,936 | 29.5 | |
| Offers accepted | 67 | 0.12 | |
| Offers declined | 159 | 0.29 | |
| Offers lost | 2 | 0.00 | |
| Offers received | 233 | 0.43 | |
| Deliveries completed | 75 | 0.14 | 500 |

Couriers online at the end: **121**. Intended demand at the end (scenario curve × weather): **206 orders/h**.

## SLOs

| SLO | Target | Measured | Verdict |
|---|---:|---:|---|
| Quote and order requests succeed (not 5xx) | 99.9 % | 100.000 % (0 of 78 failed) | met |
| `POST /v1/orders` under 300 ms | 95 % | 100.00 % | met |
| GPS points in live state within 2 s | 99 % | 96.49 % | **missed** |
| Viewers see positions younger than 10 s | 99 % | 100.00 % | met |
| A courier accepts within 60 s | 95 % | 7.14 % | **missed** |

## Latency

Server side (`http_request_duration_seconds`, what the SLO measures):

| Endpoint | Requests/s | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| POST /v1/quotes (orders) | 0.1 | 17 ms | 202 ms | 280 ms |
| POST /v1/orders (orders) | 0.1 | 14 ms | 24 ms | 40 ms |
| POST /v1/locations (fleet-ingest) | 29.5 | 17 ms | 24 ms | 25 ms |

Client side, as the simulator saw it (includes the network and any queueing):

| Call | Requests/s | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| `accept` | 0.1 | 5 ms | 10 ms | 10 ms |
| `decline` | 0.3 | 5 ms | 10 ms | 17 ms |
| `deliver` | 0.1 | 13 ms | 43 ms | 81 ms |
| `locations` | 29.5 | 17 ms | 24 ms | 25 ms |
| `order` | 0.1 | 14 ms | 24 ms | 40 ms |
| `pickup` | 0.2 | 17 ms | 47 ms | 98 ms |
| `quote` | 0.1 | 19 ms | 202 ms | 280 ms |
| `route` | 0.3 | 5 ms | 16 ms | 23 ms |
| `track_stream` | 0.0 | 312 ms | 825 ms | 965 ms |

## Location pipeline and dispatch

| Signal | Value |
|---|---:|
| GPS points accepted by fleet-ingest | 29.5/s |
| fleet-ingest requests shed (503) | 0 |
| Location freshness p99 | 20.0 s |
| Tracking event age p99 | 0.2 s |
| Tracking SSE connections at the end | 44 |
| Tracking events dropped (slow viewers) | 0 |
| Assignment time p50 / p95 (assigned orders) | 12.5 s / 28.5 s |
| Orders with no courier (first report) | 39 of 42 |
| Dispatch offers accepted | 67 |
| Dispatch offers declined | 152 |
| Dispatch offers expired | 0 |
| Dispatch offers sent | 233 |
| Dispatch offers withdrawn | 9 |
| Dispatch pending orders at the end | 55 |
| Dispatch offer streams open at the end | 121 |
| ETA requests from `fallback` | 269 |
| ETA requests from `osrm` | 0 |

## What viewers saw

Customers with the tracking page open, measured by the simulator (`simulator_tracking_*`).

| Signal | Value |
|---|---:|
| Tracking pages open at the end | 44 |
| `position` events received | 202 (0.4/s) |
| `status` events received | 31 (0.1/s) |
| Position age when received p50 / p99 | 0.1 s / 15.0 s |
| Positions younger than 10 s | 96.53 % |
| Couriers by state at the end | at_store 9, idle 8, to_dropoff 37, to_store 67 |

## Resources at the end of the window

`docker stats` (CPU is per core: 100 % = one core).

| Container | CPU | Memory |
|---|---:|---|
| tasuki-delivery-platform-console-1 | 0.00% | 9.18MiB / 7.748GiB |
| tasuki-delivery-platform-dispatch-1 | 4.30% | 164.2MiB / 7.748GiB |
| tasuki-delivery-platform-fleet-ingest-1 | 2.16% | 88.06MiB / 7.748GiB |
| tasuki-delivery-platform-fleet-state-1 | 1.12% | 76.05MiB / 7.748GiB |
| tasuki-delivery-platform-kafka-1 | 6.93% | 1.144GiB / 7.748GiB |
| tasuki-delivery-platform-orders-1 | 0.04% | 152.5MiB / 7.748GiB |
| tasuki-delivery-platform-orders-consumer-1 | 2.36% | 62.31MiB / 7.748GiB |
| tasuki-delivery-platform-orders-db-1 | 6.06% | 122.5MiB / 7.748GiB |
| tasuki-delivery-platform-orders-relay-1 | 0.95% | 61.55MiB / 7.748GiB |
| tasuki-delivery-platform-redis-1 | 0.55% | 27.14MiB / 7.748GiB |
| tasuki-delivery-platform-tracking-1 | 1.01% | 199.6MiB / 7.748GiB |
| tasuki-loadtest-simulator | 1.99% | 32.68MiB / 7.748GiB |

## Timeline

One line per sample: the rate and SLIs over that interval only.

```text
16:37:09  orders/h 717 · GPS pts/s 32 · POST /v1/orders p95 10 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:37:19  orders/h 1,076 · GPS pts/s 29 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:37:29  orders/h 0 · GPS pts/s 32 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:37:39  orders/h 359 · GPS pts/s 29 · POST /v1/orders p95 10 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:37:49  orders/h 0 · GPS pts/s 32 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:38:00  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:38:10  orders/h 358 · GPS pts/s 32 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:38:20  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:38:30  orders/h 1,077 · GPS pts/s 32 · POST /v1/orders p95 23 ms · location fresh ≤2s n/a · assigned ≤60s n/a
16:38:40  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 32.77 % · assigned ≤60s n/a
16:38:50  orders/h 359 · GPS pts/s 32 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:39:00  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:39:10  orders/h 717 · GPS pts/s 32 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:39:20  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:39:30  orders/h 0 · GPS pts/s 32 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:39:40  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:39:50  orders/h 356 · GPS pts/s 31 · POST /v1/orders p95 49 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:40:00  orders/h 359 · GPS pts/s 30 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:40:10  orders/h 353 · GPS pts/s 31 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:40:20  orders/h 359 · GPS pts/s 29 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:40:30  orders/h 358 · GPS pts/s 31 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:40:40  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:40:51  orders/h 0 · GPS pts/s 31 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:41:01  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:41:11  orders/h 0 · GPS pts/s 31 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:41:21  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:41:31  orders/h 358 · GPS pts/s 31 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:41:41  orders/h 1,074 · GPS pts/s 29 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:41:51  orders/h 358 · GPS pts/s 31 · POST /v1/orders p95 10 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:42:01  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:42:11  orders/h 717 · GPS pts/s 31 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:42:21  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:42:31  orders/h 359 · GPS pts/s 30 · POST /v1/orders p95 10 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:42:41  orders/h 359 · GPS pts/s 29 · POST /v1/orders p95 10 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:42:51  orders/h 0 · GPS pts/s 30 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:43:01  orders/h 359 · GPS pts/s 29 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:43:11  orders/h 0 · GPS pts/s 31 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:43:21  orders/h 0 · GPS pts/s 28 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:43:31  orders/h 0 · GPS pts/s 30 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:43:41  orders/h 359 · GPS pts/s 27 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:43:51  orders/h 0 · GPS pts/s 30 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:44:01  orders/h 717 · GPS pts/s 27 · POST /v1/orders p95 10 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:44:11  orders/h 0 · GPS pts/s 30 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s 100.00 %
16:44:21  orders/h 0 · GPS pts/s 27 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:44:32  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:44:42  orders/h 356 · GPS pts/s 26 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:44:52  orders/h 718 · GPS pts/s 29 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:45:02  orders/h 359 · GPS pts/s 27 · POST /v1/orders p95 10 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:45:12  orders/h 0 · GPS pts/s 30 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s 50.00 %
16:45:22  orders/h 0 · GPS pts/s 27 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:45:32  orders/h 0 · GPS pts/s 30 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:45:42  orders/h 715 · GPS pts/s 26 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s n/a
16:45:52  orders/h 717 · GPS pts/s 29 · POST /v1/orders p95 24 ms · location fresh ≤2s 100.00 % · assigned ≤60s 25.00 %
16:45:59  orders/h 0 · GPS pts/s 29 · POST /v1/orders p95 n/a · location fresh ≤2s 100.00 % · assigned ≤60s n/a
```
