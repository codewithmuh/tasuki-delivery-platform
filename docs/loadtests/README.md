# Load tests

Published results of load tests against Tasuki, with what was run, on what, and how to reproduce it. A number on
the [landing page](../../website/) or in the [README](../../README.md) must come from a report here.

## How a run works

1. **The platform** runs as the local Compose stack (`make up`): one replica of every service, one Kafka broker, one
   PostgreSQL, one Valkey. No OSRM unless stated: dispatch and couriers then use straight-line distances.
2. **The load** comes from the [simulator](../../services/simulator/README.md), the same program that plays the city in
   every environment. It drives only the public APIs: customers get quotes and place orders (open loop: an order is
   placed when it is due, whatever the latency of the last one), couriers stream GPS, take offers and deliver, and
   60 % of customers keep the tracking page open until delivery (`demand.watch_share`).
3. **The measurement** is [`scripts/loadtest/report.py`](../../scripts/loadtest/report.py). The local stack has no
   Prometheus, so the script scrapes every service's `/metrics` at the start and end of the window (and once per
   interval for the timeline) and computes rates, percentiles and SLIs from the differences, as PromQL's `rate()`
   and `histogram_quantile()` would. The SLIs and targets are the production ones from
   [observability/slo](../../observability/slo/README.md).
4. **The runner** is [`scripts/loadtest/run.sh`](../../scripts/loadtest/run.sh): it starts the simulator in the given
   scenario at a given simulated time of day (`-start 11:45` begins a rainy Friday in the rain), waits for the
   warm-up, measures, and stops the simulator.

```bash
make up                                   # the platform
scripts/loadtest/run.sh --scenario rainy-friday --start 11:45 --warmup 300 --window 900 \
  --label "rainy-friday, rain peak" --out docs/loadtests/$(date +%F)-rainy-friday-local.md
```

## Read the numbers honestly

- **One machine runs everything**, the load generator included. The simulator competes with the platform for the
  same CPUs, so a laptop result is a floor for what the services can do, not a ceiling.
- **Percentiles are bucket estimates.** Like Prometheus, the script interpolates inside histogram buckets; a p95 of
  "120 ms" means "between the 100 ms and 200 ms bounds, most likely about 120 ms".
- **Dispatch speed measures supply as well as software.** In the rain, demand outruns couriers on purpose (design
  §4: a surge is a capacity problem). Orders that wait for a courier burn the dispatch SLO with the software
  working exactly as designed. The report separates the two: assignment time for assigned orders, and the count of
  orders with no courier.
- **Each report states its window.** Rates over 15 minutes at the rain peak are not a day's average.

## Results

| Date | Report | Scenario | Where | Headline |
|---|---|---|---|---|
| 2026-10-16 | [Full design scale](2026-10-16-design-scale-local.md) | `rainy-friday-design-scale` from 11:45 (rain) | One laptop, Compose | 18,144 orders/h, 641 GPS points/s, 2,707 open tracking pages; 0 failed orders, `POST /v1/orders` 20 ms p95 |
| 2026-10-16 | [One-tenth scale](2026-10-16-rainy-friday-local.md) | `rainy-friday` from 11:45 (rain) | One laptop, Compose | 1,832 orders/h, 55 GPS points/s; 0 failed orders, 23 ms p95 |

In both runs every software SLO was met and dispatch speed was missed because demand outran couriers, as design §4
predicts for a rain surge. Each report ends with its findings.
