# Alerts (non-SLO)

SLO burn-rate alerts are generated from the Sloth specs in [../slo/](../slo/). This folder holds the other alerts:
causes the SLOs cannot see in time, and the error-budget policy's stages. They are plain Prometheus rule groups,
checked and unit-tested by `make slo-check`, and wrapped in `PrometheusRule` objects by the GitOps layer.

**Rule of thumb:** page on symptoms (SLO burn), open tickets for causes. There are three exceptions, each written down
next to its rule. They page because a component that stops completely produces no SLI events, so its SLO goes quiet
instead of burning.

| Alert | Severity | Fires when | Runbook |
|---|---|---|---|
| `OrdersOutboxLagHigh` | page | Oldest unpublished outbox row older than 60 s for 5 min | [orders-outbox-lag](../../docs/runbooks/orders-outbox-lag.md) |
| `KafkaConsumerLagHigh` | page | fleet-state over 7,500 messages behind on `courier.locations` for 5 min | [kafka-consumer-lag](../../docs/runbooks/kafka-consumer-lag.md) |
| `TrackingFeedStalled` | page | A tracking replica with console viewers sends nothing for 5 min while couriers report | [tracking-feed-stalled](../../docs/runbooks/tracking-feed-stalled.md) |
| `TrackingSSEDropsHigh` | ticket | Over 2 % of position events dropped for slow viewers for 15 min | [tracking-sse-drops](../../docs/runbooks/tracking-sse-drops.md) |
| `ETAFallbackHigh` | ticket | Over 10 % of ETAs from a fallback (orders: dispatch did not answer; dispatch: OSRM failed) for 10 min | [eta-fallback](../../docs/runbooks/eta-fallback.md) |
| `SLOErrorBudgetLow` | ticket | An SLO has 25 % or less of its 28-day budget left | [error-budget-low](../../docs/runbooks/error-budget-low.md) |
| `SLOErrorBudgetExhausted` | ticket | An SLO's budget is spent | [error-budget-exhausted](../../docs/runbooks/error-budget-exhausted.md) |
| `RolloutGateBudgetUnknown` | ticket | The rollout gate decided without a budget (Prometheus error or no data) | [rollout-gate-budget-unknown](../../docs/runbooks/rollout-gate-budget-unknown.md) |

## Metrics

| Metric | Type | Exported by | Status |
|---|---|---|---|
| `orders_outbox_lag_seconds` | gauge | orders outbox relay | exported (`services/orders/src/core/metrics.py`) |
| `orders_quotes_created_total{eta_source="dispatch"\|"fallback"}` | counter | orders | exported |
| `tracking_event_age_seconds_count{stream}` | histogram count | tracking | exported (`services/fleet/internal/tracking/feed.go`); counts events sent |
| `tracking_sse_dropped_events_total{stream}`, `tracking_sse_connections{stream}` | counter, gauge | tracking | exported |
| `fleet_ingest_points_total{result}` | counter | fleet-ingest | exported |
| `rollout_gate_decisions_total{allowed,cause}` | counter | rollout-gate | exported (`tools/rollout-gate`) |
| `kafka_consumergroup_lag{consumergroup,topic,partition}` | gauge | Kafka Exporter (ships with Strimzi) | **assumed**: depends on the Kafka choice (ADR 0004) |
| `dispatch_eta_requests_total{source="osrm"\|"fallback"}` | counter | dispatch | confirmed by the dispatch agent (one per road-time lookup) |

Only fleet-state consumes `courier.locations` in a consumer group (`KAFKA_GROUP`, default `fleet-state`). Tracking reads
it without a group, so its health comes from its own metrics (`TrackingFeedStalled`). If dev and prod share one Kafka
cluster, give them different `KAFKA_GROUP` values.
