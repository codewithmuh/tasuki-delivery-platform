# Load test: rainy-friday at full design scale, rain peak, one laptop

| | |
|---|---|
| Started | 2026-10-16 11:14:15 UTC |
| Window | 15.0 min (900 s) |
| Scenario | rainy-friday-design-scale, starting at 11:45 simulated time |
| Measured by | `scripts/loadtest/report.py` (scrapes every service's `/metrics`; PromQL-equivalent maths) |

## Load the simulator generated

| What | Total | Per second | Per hour |
|---|---:|---:|---:|
| Orders attempted | 4,536 | 5.04 | 18,144 |
| Orders created | 4,536 | 5.04 | 18,144 |
| GPS points accepted | 577,077 | 641.2 | |
| GPS points dropped | 0 | 0.0 | |
| GPS batches sent | 577,077 | 641.2 | |
| Offers accepted | 2,632 | 2.92 | |
| Offers declined | 4,269 | 4.74 | |
| Offers received | 6,928 | 7.70 | |
| Deliveries completed | 1,695 | 1.88 | 6,780 |

Couriers online at the end: **2,694**. Intended demand at the end (scenario curve × weather): **18,000 orders/h**.

## SLOs

| SLO | Target | Measured | Verdict |
|---|---:|---:|---|
| Quote and order requests succeed (not 5xx) | 99.9 % | 100.000 % (0 of 9,072 failed) | met |
| `POST /v1/orders` under 300 ms | 95 % | 99.98 % | met |
| GPS points in live state within 2 s | 99 % | 100.00 % | met |
| Viewers see positions younger than 10 s | 99 % | 100.00 % | met |
| A courier accepts within 60 s | 95 % | 29.09 % | **missed** |

## Latency

Server side (`http_request_duration_seconds`, what the SLO measures):

| Endpoint | Requests/s | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| POST /v1/quotes (orders) | 5.0 | 7 ms | 16 ms | 32 ms |
| POST /v1/orders (orders) | 5.0 | 8 ms | 20 ms | 25 ms |
| POST /v1/locations (fleet-ingest) | 641.2 | 16 ms | 24 ms | 25 ms |

Client side, as the simulator saw it (includes the network and any queueing):

| Call | Requests/s | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| `accept` | 2.9 | 5 ms | 10 ms | 10 ms |
| `decline` | 4.7 | 5 ms | 10 ms | 10 ms |
| `deliver` | 1.9 | 8 ms | 24 ms | 47 ms |
| `locations` | 641.2 | 17 ms | 24 ms | 25 ms |
| `order` | 5.0 | 6 ms | 22 ms | 30 ms |
| `pickup` | 2.9 | 8 ms | 25 ms | 49 ms |
| `quote` | 5.0 | 6 ms | 20 ms | 40 ms |
| `route` | 5.8 | 5 ms | 14 ms | 24 ms |
| `track_stream` | 3.0 | 356 ms | 892 ms | 978 ms |

## Location pipeline and dispatch

| Signal | Value |
|---|---:|
| GPS points accepted by fleet-ingest | 641.2/s |
| fleet-ingest requests shed (503) | 0 |
| Location freshness p99 | 0.0 s |
| Tracking event age p99 | 0.2 s |
| Tracking SSE connections at the end | 2,707 |
| Tracking events dropped (slow viewers) | 0 |
| Assignment time p50 / p95 (assigned orders) | 8.2 s / 45.9 s |
| Orders with no courier (first report) | 3,016 of 4,283 |
| Dispatch offers accepted | 2,633 |
| Dispatch offers declined | 4,269 |
| Dispatch offers expired | 0 |
| Dispatch offers sent | 6,928 |
| Dispatch offers withdrawn | 0 |
| Dispatch pending orders at the end | 1,935 |
| Dispatch offer streams open at the end | 2,694 |
| ETA requests from `fallback` | 11,464 |
| ETA requests from `osrm` | 0 |

## What viewers saw

Customers with the tracking page open, measured by the simulator (`simulator_tracking_*`).

| Signal | Value |
|---|---:|
| Tracking pages open at the end | 2,707 |
| `end` events received | 888 (1.0/s) |
| `position` events received | 326,662 (363.0/s) |
| `status` events received | 6,552 (7.3/s) |
| Streams finished: `ended` | 888 |
| Position age when received p50 / p99 | 0.1 s / 0.2 s |
| Positions younger than 10 s | 99.76 % |
| Couriers by state at the end | at_store 368, idle 58, to_dropoff 913, to_store 1,355 |

## Resources at the end of the window

`docker stats` (CPU is per core: 100 % = one core).

| Container | CPU | Memory |
|---|---:|---|
| tasuki-delivery-platform-console-1 | 0.00% | 9.18MiB / 7.748GiB |
| tasuki-delivery-platform-dispatch-1 | 16.62% | 196.6MiB / 7.748GiB |
| tasuki-delivery-platform-fleet-ingest-1 | 12.74% | 82.74MiB / 7.748GiB |
| tasuki-delivery-platform-fleet-state-1 | 4.61% | 72.28MiB / 7.748GiB |
| tasuki-delivery-platform-kafka-1 | 33.71% | 1.098GiB / 7.748GiB |
| tasuki-delivery-platform-orders-1 | 8.14% | 152.4MiB / 7.748GiB |
| tasuki-delivery-platform-orders-consumer-1 | 3.22% | 61.32MiB / 7.748GiB |
| tasuki-delivery-platform-orders-db-1 | 4.28% | 120.5MiB / 7.748GiB |
| tasuki-delivery-platform-orders-relay-1 | 1.33% | 60.92MiB / 7.748GiB |
| tasuki-delivery-platform-redis-1 | 13.28% | 33.97MiB / 7.748GiB |
| tasuki-delivery-platform-tracking-1 | 8.22% | 216.6MiB / 7.748GiB |
| tasuki-loadtest-simulator | 13.35% | 436.9MiB / 7.748GiB |

## Timeline

One line per sample: the rate and SLIs over that interval only.

```text
16:15:15  orders/h 17,750 · GPS pts/s 511 · POST /v1/orders p95 18 ms · location fresh ≤2s 100.00 % · assigned ≤60s 97.99 %
16:16:15  orders/h 16,718 · GPS pts/s 561 · POST /v1/orders p95 16 ms · location fresh ≤2s 100.00 % · assigned ≤60s 93.10 %
16:17:15  orders/h 18,645 · GPS pts/s 611 · POST /v1/orders p95 10 ms · location fresh ≤2s 100.00 % · assigned ≤60s 82.49 %
16:18:15  orders/h 17,987 · GPS pts/s 652 · POST /v1/orders p95 20 ms · location fresh ≤2s 100.00 % · assigned ≤60s 64.09 %
16:19:15  orders/h 18,229 · GPS pts/s 668 · POST /v1/orders p95 20 ms · location fresh ≤2s 100.00 % · assigned ≤60s 17.19 %
16:20:16  orders/h 18,226 · GPS pts/s 668 · POST /v1/orders p95 18 ms · location fresh ≤2s 100.00 % · assigned ≤60s 5.69 %
16:21:16  orders/h 17,269 · GPS pts/s 667 · POST /v1/orders p95 19 ms · location fresh ≤2s 100.00 % · assigned ≤60s 7.89 %
16:22:16  orders/h 17,448 · GPS pts/s 665 · POST /v1/orders p95 17 ms · location fresh ≤2s 100.00 % · assigned ≤60s 9.76 %
16:23:16  orders/h 17,808 · GPS pts/s 662 · POST /v1/orders p95 18 ms · location fresh ≤2s 100.00 % · assigned ≤60s 9.69 %
16:24:16  orders/h 16,544 · GPS pts/s 659 · POST /v1/orders p95 21 ms · location fresh ≤2s 100.00 % · assigned ≤60s 11.45 %
16:25:16  orders/h 18,830 · GPS pts/s 657 · POST /v1/orders p95 25 ms · location fresh ≤2s 100.00 % · assigned ≤60s 12.28 %
16:26:16  orders/h 18,943 · GPS pts/s 661 · POST /v1/orders p95 21 ms · location fresh ≤2s 100.00 % · assigned ≤60s 8.65 %
16:27:16  orders/h 19,849 · GPS pts/s 658 · POST /v1/orders p95 23 ms · location fresh ≤2s 100.00 % · assigned ≤60s 10.12 %
16:28:16  orders/h 19,907 · GPS pts/s 660 · POST /v1/orders p95 22 ms · location fresh ≤2s 100.00 % · assigned ≤60s 11.47 %
16:29:15  orders/h 18,008 · GPS pts/s 657 · POST /v1/orders p95 15 ms · location fresh ≤2s 100.00 % · assigned ≤60s 16.16 %
```

## Findings

Written after the run, from the numbers above and readings taken during it.

1. **One laptop carried the design's full rain peak.** 18,144 orders/h (5.04 orders/s, the design's ×3 rainy lunch),
   2,694 couriers online, 641 GPS points/s and 2,707 open tracking pages, with every software SLO met: no quote or
   order failed (0 of 9,072), `POST /v1/orders` was 20 ms at p95 (99.98 % under 300 ms), GPS batches were 24 ms at
   p95 with nothing shed, every point reached live state within 2 s, and viewers received positions 0.2 s old at p99.
   The simulator's README called this scenario "cluster only"; this run shows the platform itself does not need the
   cluster for this load. What the cluster adds is replicas, zones and headroom, not raw capacity.
2. **Kafka is the heaviest component; the services are light.** At the peak (a reading at 16:20) Kafka used about
   1.15 cores; dispatch 16 % of a core, orders 11 %, fleet-ingest 10 %, tracking 7 %. Every service stayed
   under 220 MiB of memory (tracking, holding 2,707 streams, was the largest at 217 MiB). Sizing for production should start from the broker, not the services.
3. **Deliveries matched the design's capacity estimate again.** 6,780 deliveries/h from about 2,700 couriers, against
   design §4's 7,200/h for 3,000. Dispatch speed was missed for the same reason as the one-tenth run: demand
   (18,000/h) is two and a half times what the couriers can carry. Assigned orders were matched in 8.2 s at p50.
4. **Half of the busy couriers were waiting for food.** A reading at 16:20 showed 1,300 of 2,664 busy couriers
   `at_store`. Prep-aware offers (offer the order so the courier arrives when it is ready) are the biggest capacity
   lever this test found. Recorded as an input to batched dispatch (roadmap phase 6).
5. **The tracking freshness SLI counts live events only.** Tracking's own histogram says 100 % of positions were
   under 10 s old; viewers measured 99.76 %. The difference is the first position a viewer gets when it connects
   (the courier's last known point, sent in the snapshot), which the SLI does not count. For a courier waiting at a
   store (GPS every 15 s when idle) that point can be up to 15 s old. It reflects GPS cadence, not pipeline delay,
   and the event carries `received_at`, so a page can show "updated 12 s ago". Decision for now: keep the SLI as it
   is and say so in the SLO documentation; revisit if customers see stale maps.
6. **No OSRM in this run** (every ETA used the straight-line fallback), and **one machine ran the load generator
   too**, so these numbers are a floor for the platform, not a ceiling.
