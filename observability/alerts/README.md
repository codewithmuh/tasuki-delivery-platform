# Alerts (non-SLO)

SLO burn-rate alerts are generated from the Sloth specs in [../slo/](../slo/). This folder holds the other alerts:
causes the SLOs cannot see in time, the platform pieces the services depend on, and the error-budget policy's
stages. They are plain Prometheus rule groups, checked and unit-tested by `make slo-check`, and wrapped in
`PrometheusRule` objects by the GitOps layer (one Argo CD Application per file, so a new file needs no change in
`deploy/`).

**Rule of thumb:** page on symptoms (SLO burn), open tickets for causes. The exceptions page because a component
that stops completely produces no SLI events, so its SLO goes quiet instead of burning: a stalled consumer or relay,
a paused dispatch, couriers that cannot report, a database without a primary (orders pods go unready after 120 s, and
requests that never reach Django are not in the SLI), Kafka unable to take writes. Each one says so next to its rule.

**Environments.** Service alerts select `namespace="tasuki-prod"` and keep `namespace` in their `by`, so dev and prod
are never added together and the Alertmanager knows which environment an alert is about. Platform-plumbing alerts
(secrets, backups) cover both environments, because a broken pipeline shows up in dev first. The Alertmanager sends
every `tasuki-dev` alert to the ticket channel, so dev never pages ([deploy/README.md](../../deploy/README.md#alerting)).

Every rule has `severity` (`page` or `ticket`), `category` (dashboards count firing alerts by it) and a `runbook_url`;
the runbook index is [docs/runbooks/README.md](../../docs/runbooks/README.md).

| Alert | Severity | Fires when | Runbook |
|---|---|---|---|
| `OrdersOutboxLagHigh` | page | Oldest unpublished outbox row older than 60 s for 5 min | [orders-outbox-lag](../../docs/runbooks/orders-outbox-lag.md) |
| `KafkaConsumerLagHigh` | page | fleet-state over 7,500 messages behind on `courier.locations` for 5 min | [kafka-consumer-lag](../../docs/runbooks/kafka-consumer-lag.md) |
| `KafkaConsumerStalled` | page (dispatch), ticket (orders) | A partition with messages waiting and no commit for 5 min, for 2 / 10 min | [kafka-consumer-lag](../../docs/runbooks/kafka-consumer-lag.md) |
| `KafkaUnavailable` | page | Fewer than 2 brokers ready (min in-sync replicas) for 2 min | [kafka-unavailable](../../docs/runbooks/kafka-unavailable.md) |
| `KafkaUnderReplicated` | ticket | Under-replicated partitions for 15 min | [kafka-unavailable](../../docs/runbooks/kafka-unavailable.md) |
| `FleetCacheUnavailable` | page | Valkey not ready, no fleet-state replica ready, or dispatch paused, for 5 min (`component`) | [redis-lost](../../docs/runbooks/redis-lost.md) |
| `CourierLocationsLost` | page | Couriers swept without going offline in 10 min > a fifth of the online fleet (≥ 5) | [courier-locations-lost](../../docs/runbooks/courier-locations-lost.md) |
| `CourierTokensRejected` | page | 401s from fleet-ingest or dispatch in 10 min > a fifth of the online fleet (≥ 10) | [courier-locations-lost](../../docs/runbooks/courier-locations-lost.md) |
| `FleetIngestShedding` | ticket | Over 5 % of GPS points refused with 503 for 15 min | [courier-locations-lost](../../docs/runbooks/courier-locations-lost.md) |
| `TrackingFeedStalled` | page | A tracking replica with console viewers sends nothing for 5 min while couriers report | [tracking-feed-stalled](../../docs/runbooks/tracking-feed-stalled.md) |
| `TrackingSSEDropsHigh` | ticket | Over 2 % of position events dropped for slow viewers for 15 min | [tracking-sse-drops](../../docs/runbooks/tracking-sse-drops.md) |
| `ETAFallbackHigh` | ticket | Over 10 % of ETAs from a fallback (orders: dispatch did not answer; dispatch: OSRM failed) for 10 min | [eta-fallback](../../docs/runbooks/eta-fallback.md) |
| `OrdersDatabaseUnavailable` | page | No orders-db instance accepting writes for 2 min | [postgres-failover](../../docs/runbooks/postgres-failover.md) |
| `OrdersDatabaseFailover` | ticket | A standby became the primary in the last 15 min | [postgres-failover](../../docs/runbooks/postgres-failover.md) |
| `OrdersDatabaseReplicationDegraded` | ticket | No streaming standby for 10 min, or standby over 30 s behind for 5 min (`problem`) | [postgres-failover](../../docs/runbooks/postgres-failover.md) |
| `OrdersDatabaseBackupFailing` | ticket | Last base backup failed, none recent (3 h prod, 26 h dev), or WAL archiving failing (`problem`) | [backup-restore](../../docs/runbooks/backup-restore.md) |
| `ExternalSecretSyncFailing` | ticket | An ExternalSecret not Ready for 10 min | [broken-secret-stuck-rollout](../../docs/runbooks/broken-secret-stuck-rollout.md) |
| `RolloutDegraded` | ticket | An Argo Rollout aborted or not progressing, for 5 min | [broken-secret-stuck-rollout](../../docs/runbooks/broken-secret-stuck-rollout.md) |
| `SLOErrorBudgetLow` | ticket | An SLO has 25 % or less of its 28-day budget left | [error-budget-low](../../docs/runbooks/error-budget-low.md) |
| `SLOErrorBudgetExhausted` | ticket | An SLO's budget is spent | [error-budget-exhausted](../../docs/runbooks/error-budget-exhausted.md) |
| `RolloutGateBudgetUnknown` | ticket | The rollout gate decided without a budget (Prometheus error or no data) | [rollout-gate-budget-unknown](../../docs/runbooks/rollout-gate-budget-unknown.md) |

**Not duplicated here:** kube-prometheus-stack's defaults already cover crash-looping, Pending and unready pods,
Deployments that stop progressing, nodes that go NotReady, and volumes filling up (`KubePodCrashLooping`,
`KubePodNotReady`, `KubeDeploymentRolloutStuck`, `KubeNodeNotReady`, `KubePersistentVolumeFillingUp`, ...). The
runbook index maps them to Tasuki runbooks.

## Metrics

| Metric | Type | Exported by | Status |
|---|---|---|---|
| `orders_outbox_lag_seconds` | gauge | orders outbox relay | exported (`services/orders/src/core/metrics.py`) |
| `orders_quotes_created_total{eta_source="dispatch"\|"fallback"}` | counter | orders | exported |
| `tracking_event_age_seconds_count{stream}` | histogram count | tracking | exported (`services/fleet/internal/tracking/feed.go`); counts events sent |
| `tracking_sse_dropped_events_total{stream}`, `tracking_sse_connections{stream}` | counter, gauge | tracking | exported |
| `fleet_ingest_points_total{result}` | counter | fleet-ingest | exported (`services/fleet/internal/ingest/handler.go`) |
| `http_requests_total{service,status}` | counter | every service | exported (`services/fleet/internal/kit/server.go`, `services/orders/src/core/metrics.py`) |
| `fleet_state_swept_couriers_total` | counter | fleet-state | exported (`services/fleet/internal/state/processor.go`) |
| `dispatch_couriers{state}`, `dispatch_paused` | gauges | dispatch | exported (`services/fleet/internal/dispatch/metrics.go`) |
| `dispatch_eta_requests_total{source="osrm"\|"fallback"}` | counter | dispatch | exported |
| `rollout_gate_decisions_total{allowed,cause}` | counter | rollout-gate | exported (`tools/rollout-gate`) |
| `kafka_consumergroup_lag`, `kafka_consumergroup_current_offset{consumergroup,topic,partition}`, `kafka_topic_partition_under_replicated_partition{topic,partition}` | gauges | Kafka Exporter (Strimzi `kafkaExporter`, PodMonitor `kafka`) | names from the exporter's documentation; **verify on a cluster** |
| `cnpg_pg_replication_{in_recovery,lag,streaming_replicas}`, `cnpg_pg_stat_archiver_{archived,failed}_count`, `cnpg_collector_up`, `cnpg_collector_pg_wal_archive_status{value}` | gauges, counters | CloudNativePG instance manager (PodMonitor `orders-db`) | names from CloudNativePG's default monitoring queries and docs; **verify on a cluster** |
| `barman_cloud_cloudnative_pg_io_last_{available,failed}_backup_timestamp` | gauges | Barman Cloud plugin v0.15.1, through the instance manager | names from the plugin's docs and source; **verify on a cluster** |
| `externalsecret_status_condition{name,namespace,condition,status}` | gauge | External Secrets v2.12 (ServiceMonitor) | names from its docs and source; the object's namespace may arrive as `exported_namespace` |
| `rollout_info{namespace,name,phase}` | gauge | Argo Rollouts controller (ServiceMonitor) | names from its source; same `exported_namespace` caveat |
| `kube_pod_status_ready`, `kube_pod_info`, `kube_statefulset_status_replicas_ready`, `kube_deployment_status_replicas_available` | gauges | kube-state-metrics (kube-prometheus-stack) | standard |

Only fleet-state consumes `courier.locations` in a consumer group (`KAFKA_GROUP`, default `fleet-state`). Tracking reads
it without a group, so its health comes from its own metrics (`TrackingFeedStalled`). dispatch consumes `orders.events`
(group `dispatch`) and orders consumes `dispatch.events` (group `orders`).
