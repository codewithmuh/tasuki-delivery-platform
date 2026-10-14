# Runbooks

One page per alert. Every alert annotation must link to its runbook.

| Alert | Severity | Runbook |
|---|---|---|
| Service unavailable / readiness failing | — | [service-unavailable.md](service-unavailable.md) |
| `OrdersAvailabilityBudgetBurn` | page, ticket | [slo-orders-availability.md](slo-orders-availability.md) |
| `OrdersLatencyBudgetBurn` | page, ticket | [slo-orders-latency.md](slo-orders-latency.md) |
| `FleetStateLocationFreshnessBudgetBurn` | page, ticket | [slo-fleet-state-location-freshness.md](slo-fleet-state-location-freshness.md) |
| `TrackingFreshnessBudgetBurn` | page, ticket | [slo-tracking-freshness.md](slo-tracking-freshness.md) |
| `DispatchAssignmentSpeedBudgetBurn` | page, ticket | [slo-dispatch-assignment-speed.md](slo-dispatch-assignment-speed.md) |
| `OrdersOutboxLagHigh` | page | [orders-outbox-lag.md](orders-outbox-lag.md) |
| `KafkaConsumerLagHigh` | page | [kafka-consumer-lag.md](kafka-consumer-lag.md) |
| `TrackingFeedStalled` | page | [tracking-feed-stalled.md](tracking-feed-stalled.md) |
| `TrackingSSEDropsHigh` | ticket | [tracking-sse-drops.md](tracking-sse-drops.md) |
| `ETAFallbackHigh` | ticket | [eta-fallback.md](eta-fallback.md) |
| `SLOErrorBudgetLow` | ticket | [error-budget-low.md](error-budget-low.md) |
| `SLOErrorBudgetExhausted` | ticket | [error-budget-exhausted.md](error-budget-exhausted.md) |
| `RolloutGateBudgetUnknown` | ticket | [rollout-gate-budget-unknown.md](rollout-gate-budget-unknown.md) |

Each runbook answers: **what is broken, how bad is it, how to confirm, how to mitigate, how to escalate.**

`page` wakes someone up: the budget is burning fast enough that it cannot wait. `ticket` means fix it within working
hours. SLO alerts come from [observability/slo](../../observability/slo/); the rest from
[observability/alerts](../../observability/alerts/). What happens when a budget runs out is in the
[error-budget policy](../slo/error-budget-policy.md).
