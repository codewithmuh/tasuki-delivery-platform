# GD-05: Kafka broker loss

| | |
|---|---|
| Status | **Planned, not run** |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "Kafka unavailable"; §7.1 (outbox) |
| SLOs at stake | location freshness, tracking freshness, dispatch assignment speed (the outbox delays `OrderConfirmed`); orders availability must **not** move |
| Environments | Variant A (full outage): `tasuki-dev`, whose only broker is the whole cluster. Variant B (one broker of three): `tasuki-prod` |
| Time box | 30 minutes per variant |
| Scripts | `kafka-broker-loss.sh`, `watch-slis.sh`, `record-timeline.sh` |

Kafka is Strimzi in KRaft mode: Kafka `tasuki`, node pool `dual-role` (each node is controller and broker), pods
`tasuki-dual-role-<n>`, bootstrap `tasuki-kafka-bootstrap:9092`. Dev has 1 node (RF 1); prod has 3, one per AZ
(RF 3, `min.insync.replicas` 2).

## Hypothesis

**Variant A, Kafka unavailable (dev, the only broker is deleted):**

1. **Orders are still accepted.** The API writes the order and its outbox row in one PostgreSQL transaction and never
   talks to Kafka. Quote and create success stays at steady state.
2. **The outbox waits and drains.** `orders_outbox_lag_seconds` and `orders_outbox_pending_events` rise for as long as
   Kafka is away; `orders_outbox_publish_failures_total` rises; then the backlog drains within a minute of the broker
   being Ready. No outbox row is lost or published twice in a way consumers notice (they dedupe on `event_id`).
3. **fleet-ingest sheds with `503 Retry-After: 5`** (`fleet_ingest_shed_requests_total` by reason, points with
   `result="shed"`), and stays Ready, because its readiness deliberately ignores Kafka. The simulator drops those
   batches and sends fresh ones after Retry-After (`simulator_location_batches_total{result="shed"}`). Points are lost
   on purpose.
4. **Consumers resume** when the broker returns: fleet-state's lag drains, tracking resumes sending, and dispatch (once
   it runs) sees the delayed `OrderConfirmed` events.

**Variant B, one broker of three (prod):**

5. **Nobody notices.** Partition leadership moves to the two remaining brokers, producers keep getting acknowledgements
   (2 in-sync replicas satisfy `min.insync.replicas`), and the KRaft quorum (2 of 3) holds. At most a few seconds of
   produce latency. Under-replicated partitions are above 0 until the broker is back.

**Variant C (optional, prod), two brokers of three:** below `min.insync.replicas` and below quorum: the same as
variant A, in prod, with paging. Run it only if A and B behaved as predicted:
`kafka-broker-loss.sh --context $CTX --namespace tasuki-prod --brokers 2`.

## Steady state

- **Dashboards:** Tasuki orders & outbox (outbox lag, pending, publish failures), Tasuki location pipeline (ingest
  accepted vs shed, lag), Strimzi's Kafka dashboards.
- **`watch-slis.sh`** with:

  ```bash
  --extra 'outbox pending=max(orders_outbox_pending_events{namespace="{ns}"})' \
  --extra 'outbox publish failures/s=sum(rate(orders_outbox_publish_failures_total{namespace="{ns}"}[1m]))' \
  --extra 'ingest shed by reason=sum by (reason) (rate(fleet_ingest_shed_requests_total{namespace="{ns}"}[1m]))' \
  --extra 'sim location batches by result=sum by (result) (rate(simulator_location_batches_total{namespace="{ns}"}[1m]))' \
  --extra 'under-replicated partitions=sum(kafka_topic_partition_under_replicated_partition{namespace="{ns}"})'
  ```

- `kubectl --context tasuki -n $NS get kafka,kafkanodepool` (Ready) and `get pods -l strimzi.io/cluster=tasuki`.
- **Count orders before and after** (the data-loss check): `simulator_orders_created_total` against
  `orders_created_total`, and after the drill `select count(*) from messaging_outboxevent where published_at is null`
  (0 once drained), via `kubectl exec` into the `orders-db` primary (`-c postgres -- psql -d orders`).

## Blast radius

The location path and event delivery in one namespace. Orders creation must be unaffected; if it is not, stop. In
prod variant B, nothing should be visible.

## Preconditions

- Preflight GO (prod: `--service fleet-state`, `--service tracking` and `--service orders`, each above 50 %).
- Simulator running (`small` in dev, `normal-weekday` at a meal peak in prod).
- A fleet-stream viewer, as in [GD-01](01-redis-loss.md#preconditions), to see tracking stop and resume.

## Steps

```bash
CTX=tasuki NS=tasuki-dev        # variant B: NS=tasuki-prod, export ALLOW_PROD=yes
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context $CTX --namespace $NS \
  --experiment gd-05-kafka-broker-loss --print-path)"
scripts/gameday/preflight.sh --context $CTX --namespace $NS
scripts/gameday/record-timeline.sh mark steady-state; scripts/gameday/record-timeline.sh snapshot
```

1. **Inject:** `scripts/gameday/kafka-broker-loss.sh --context $CTX --namespace $NS --brokers 1`. In dev this is every
   broker; the script says so and asks first. It then waits for Strimzi to bring the pod back and prints the times.
2. **Checkpoints while the broker is away** (a restart takes about a minute; note the real duration):
   - orders: creates succeed; outbox lag rising.
   - fleet-ingest: 503s and shed points; still Ready (`kubectl -n $NS get pods -l app.kubernetes.io/name=fleet-ingest`).
   - tracking: the fleet-stream viewer goes quiet (pings only).
3. **After the broker is Ready:** outbox lag back to 0 within a minute; shed rate 0; viewer receiving positions;
   fleet-state lag back to steady state. `mark recovered`, `snapshot`.
4. **Data check:** compare order counts (above). `note` the numbers.
5. Variant B in prod: the same command with `--namespace tasuki-prod --brokers 1`. Expect hypothesis 5.

## Expected observations

| Signal | Variant A (dev, full outage) | Variant B (prod, 1 of 3) |
|---|---|---|
| Orders create success | Unchanged | Unchanged |
| Outbox lag | Rises for the outage, then drains | Flat (seconds at most) |
| fleet-ingest | 503 + `Retry-After`, Ready throughout | No sheds |
| Tracking | Silent, then resumes | Unchanged |
| Under-replicated partitions | n/a (RF 1) | > 0 until the broker returns |

## Abort criteria

In addition to the [global ones](../README.md#abort-criteria-all-experiments):

- The broker pod is not Ready within 10 minutes (a stuck PVC, or a pod Pending because its AZ has no node).
- Order creation fails, or the outbox does not drain within 5 minutes of Kafka being Ready.
- Variant B: any producer error. Kafka should have tolerated one broker.

## Rollback

Strimzi recreates the deleted pod by itself. `kafka-broker-loss.sh --rollback` waits until every broker pod and the
Kafka resource are Ready and prints their state. If a broker stays Pending, check its PVC's zone against the nodes
(`kubectl get pvc -o wide`, `kubectl get nodes -L topology.kubernetes.io/zone`).

## Alerts and runbooks

Follow [kafka-unavailable](../../runbooks/kafka-unavailable.md) during the drill, and note every step that is wrong or slow.

| Alert | Variant A (dev, full outage) | Variant B (prod, 1 of 3) | Variant C (prod, 2 of 3) | Runbook |
|---|---|---|---|---|
| `KafkaUnavailable` (page, fewer than 2 brokers ready for 2 min) | No: rules select prod; dev never pages | No | **Yes**, after 2 minutes | [kafka-unavailable](../../runbooks/kafka-unavailable.md) |
| `CourierLocationsLost` (page) | No (prod only) | No | **Yes**, if the outage lasts long enough for fleet-state to sweep couriers | [courier-locations-lost](../../runbooks/courier-locations-lost.md) |
| `KafkaUnderReplicated` (ticket, 15 min) | No | Only if the broker stays away for 15 minutes | After 15 minutes | [kafka-unavailable](../../runbooks/kafka-unavailable.md) |
| `OrdersOutboxLagHigh`, `FleetIngestShedding`, `KafkaConsumerLagHigh` | No | No | **Inhibited** by `KafkaUnavailable` in the same namespace: visible in `ALERTS`, not sent | [orders-outbox-lag](../../runbooks/orders-outbox-lag.md), [courier-locations-lost](../../runbooks/courier-locations-lost.md) |
| `TrackingFeedStalled` | No | No | No: it requires accepted points, and ingest is shedding | [tracking-feed-stalled](../../runbooks/tracking-feed-stalled.md) |

So variant A pages nobody (dev), variant B should page nobody and open at most a `KafkaUnderReplicated` ticket, and
variant C should page once with `KafkaUnavailable`, then `CourierLocationsLost`.

## Gaps found while planning

- None open. The ones found earlier (wrong namespace and pod names in the runbooks, the wrong outbox table name, no
  "Kafka down" alert, no ingest-shedding alert, `KafkaConsumerLagHigh` without a namespace) have been addressed.
  The drill is what proves them.

## Results

<!-- fill in after the drill -->

| | |
|---|---|
| Date and window (JST) | <!-- fill in after the drill --> |
| Variant(s) | <!-- fill in after the drill --> |
| Driver / observer / scribe | <!-- fill in after the drill --> |
| Drill log | <!-- link --> |
| Hypothesis held? | <!-- per numbered point --> |

| Signal | Expected | Observed |
|---|---|---|
| Broker away (s) | ~60 | <!-- fill in after the drill --> |
| Orders create failures during the outage | 0 | <!-- fill in after the drill --> |
| Max outbox lag (s), time to drain (s) | ≈ outage, < 60 | <!-- fill in after the drill --> |
| fleet-ingest sheds, max per second; Ready throughout? | > 0; yes | <!-- fill in after the drill --> |
| Orders created: simulator vs orders service | equal | <!-- fill in after the drill --> |
| Unpublished outbox rows after recovery | 0 | <!-- fill in after the drill --> |
| Variant B producer errors | 0 | <!-- fill in after the drill --> |
| Alerts that fired | | <!-- fill in after the drill --> |

Surprises: <!-- fill in after the drill -->

Action items: <!-- fill in after the drill -->

Postmortem: <!-- link, or "not required" with the reason -->
