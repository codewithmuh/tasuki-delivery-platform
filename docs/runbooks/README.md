# Runbooks

One page per failure. Every Tasuki alert's `runbook_url` points at one of them, and every alert that can reach a
person is in the tables below.

Each runbook answers: **what is broken, how bad is it, how to confirm, how to mitigate, how to escalate.**

`page` wakes someone up: the budget is burning fast enough, or a component has stopped in a way the SLOs cannot see.
`ticket` means fix it within working hours. The Alertmanager routes every `tasuki-dev` alert to the ticket channel,
whatever its severity ([deploy/README.md](../../deploy/README.md#alerting)). SLO alerts come from
[observability/slo](../../observability/slo/); the rest from [observability/alerts](../../observability/alerts/).
What happens when a budget runs out is in the [error-budget policy](../slo/error-budget-policy.md).

## SLO burn alerts

| Alert | Severity | Runbook |
|---|---|---|
| `OrdersAvailabilityBudgetBurn` | page, ticket | [slo-orders-availability.md](slo-orders-availability.md) |
| `OrdersLatencyBudgetBurn` | page, ticket | [slo-orders-latency.md](slo-orders-latency.md) |
| `FleetStateLocationFreshnessBudgetBurn` | page, ticket | [slo-fleet-state-location-freshness.md](slo-fleet-state-location-freshness.md) |
| `TrackingFreshnessBudgetBurn` | page, ticket | [slo-tracking-freshness.md](slo-tracking-freshness.md) |
| `DispatchAssignmentSpeedBudgetBurn` | page, ticket | [slo-dispatch-assignment-speed.md](slo-dispatch-assignment-speed.md) |

## Cause and policy alerts (observability/alerts)

| Alert | Severity | Runbook |
|---|---|---|
| `OrdersOutboxLagHigh` | page | [orders-outbox-lag.md](orders-outbox-lag.md) |
| `KafkaConsumerLagHigh` | page | [kafka-consumer-lag.md](kafka-consumer-lag.md) |
| `KafkaConsumerStalled` | page (dispatch), ticket (orders) | [kafka-consumer-lag.md](kafka-consumer-lag.md) |
| `KafkaUnavailable` | page | [kafka-unavailable.md](kafka-unavailable.md) |
| `KafkaUnderReplicated` | ticket | [kafka-unavailable.md](kafka-unavailable.md) |
| `FleetCacheUnavailable` (`component` valkey, fleet-state, dispatch) | page | [redis-lost.md](redis-lost.md) |
| `CourierLocationsLost` | page | [courier-locations-lost.md](courier-locations-lost.md) |
| `CourierTokensRejected` | page | [courier-locations-lost.md](courier-locations-lost.md) |
| `FleetIngestShedding` | ticket | [courier-locations-lost.md](courier-locations-lost.md) |
| `TrackingFeedStalled` | page | [tracking-feed-stalled.md](tracking-feed-stalled.md) |
| `TrackingSSEDropsHigh` | ticket | [tracking-sse-drops.md](tracking-sse-drops.md) |
| `ETAFallbackHigh` | ticket | [eta-fallback.md](eta-fallback.md) |
| `OrdersDatabaseUnavailable` | page | [postgres-failover.md](postgres-failover.md) |
| `OrdersDatabaseFailover` | ticket | [postgres-failover.md](postgres-failover.md) |
| `OrdersDatabaseReplicationDegraded` | ticket | [postgres-failover.md](postgres-failover.md) |
| `OrdersDatabaseBackupFailing` | ticket | [backup-restore.md](backup-restore.md) |
| `ExternalSecretSyncFailing` | ticket | [broken-secret-stuck-rollout.md](broken-secret-stuck-rollout.md) |
| `RolloutDegraded` | ticket | [broken-secret-stuck-rollout.md](broken-secret-stuck-rollout.md) |
| `SLOErrorBudgetLow` | ticket | [error-budget-low.md](error-budget-low.md) |
| `SLOErrorBudgetExhausted` | ticket | [error-budget-exhausted.md](error-budget-exhausted.md) |
| `RolloutGateBudgetUnknown` | ticket | [rollout-gate-budget-unknown.md](rollout-gate-budget-unknown.md) |

## kube-prometheus-stack defaults

These ship with the chart and are not copied into `observability/alerts/`. Their `runbook_url` points at
runbooks.prometheus-operator.dev; for Tasuki namespaces, use these pages. `warning` goes to the ticket channel,
`critical` pages, `info` reaches nobody.

| Alert | Severity | Runbook |
|---|---|---|
| `KubePodNotReady`, `KubePodCrashLooping`, `KubeContainerWaiting` | warning | [broken-secret-stuck-rollout.md](broken-secret-stuck-rollout.md), [service-unavailable.md](service-unavailable.md); Pending pods: [node-az-loss.md](node-az-loss.md) |
| `KubeDeploymentRolloutStuck`, `KubeDeploymentReplicasMismatch` | warning | [broken-secret-stuck-rollout.md](broken-secret-stuck-rollout.md) |
| `KubeStatefulSetReplicasMismatch` | warning | Valkey: [redis-lost.md](redis-lost.md) |
| `KubeNodeNotReady`, `KubeNodeUnreachable` | warning | [node-az-loss.md](node-az-loss.md) |
| `KubeHpaMaxedOut` | warning | [surge-brownout.md](surge-brownout.md) |
| `KubePersistentVolumeFillingUp` | warning, critical | Kafka: [kafka-unavailable.md](kafka-unavailable.md); PostgreSQL: [backup-restore.md](backup-restore.md) (WAL piling up), [postgres-failover.md](postgres-failover.md) |
| `TargetDown` | warning | The scrape is failing, not necessarily the service; for `orders-db`, see [postgres-failover.md](postgres-failover.md) |

## Procedures without an alert of their own

| Situation | Runbook |
|---|---|
| A service is down or not ready | [service-unavailable.md](service-unavailable.md) |
| Demand surge: scaling, brownout, load shedding | [surge-brownout.md](surge-brownout.md) |
| Node or availability-zone loss, pods that cannot be placed | [node-az-loss.md](node-az-loss.md) |
| Restoring orders-db from backups | [backup-restore.md](backup-restore.md#restore-into-a-new-cluster) |

## Conventions the runbooks rely on

- **Namespaces:** `tasuki-dev` and `tasuki-prod`, each with its own Kafka (`tasuki`, pods `tasuki-dual-role-<n>`),
  PostgreSQL (`orders-db`) and Valkey (`redis-0`).
- **Workloads:** in prod, orders, fleet-ingest, tracking and dispatch are Argo Rollouts; everything else, and all of
  dev, are Deployments. Select pods with `app.kubernetes.io/name=<service>,app.kubernetes.io/component=server`.
- **Argo CD self-heals** every object it manages. Roll back, scale or reconfigure through git; `kubectl rollout
  undo`, `kubectl argo rollouts undo`, `kubectl scale` and `kubectl edit` are reverted within seconds. Safe without
  git: deleting pods, `kubectl rollout restart`, and `kubectl argo rollouts abort/retry/promote/restart`.
- **Never print a secret.** Compare hashes, list key names.
