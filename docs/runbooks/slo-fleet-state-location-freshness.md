# Runbook: fleet-state location freshness budget burn

Alert: `FleetStateLocationFreshnessBudgetBurn` (`severity: page` or `ticket`) · SLO: 99 % of GPS points in Redis
within 2 s of receipt over 28 days · [spec](../../observability/slo/specs/fleet-state.yaml)

## Symptoms

- **page:** more than 13 % of points were late over the last hour and 5 minutes, or more than 5.6 % over 6 hours and
  30 minutes.
- **ticket:** a slower burn over a day or 3 days.

## Impact

Dispatch matches orders against stale courier positions, so it may offer an order to a courier who has already
moved away. The console map lags. Tracking is not affected directly: it reads Kafka, not Redis.

## Confirm

```promql
# Lag percentiles (points written now; lag = now - received_at)
histogram_quantile(0.99, sum by (le) (rate(fleet_state_freshness_seconds_bucket{namespace="tasuki-prod"}[5m])))

# Is fleet-state behind on Kafka? (KafkaConsumerLagHigh pages when this stays high)
sum(kafka_consumergroup_lag{namespace="tasuki-prod",consumergroup="fleet-state",topic="courier.locations"})

# Throttling, per pod
sum by (pod) (rate(container_cpu_cfs_throttled_periods_total{namespace="tasuki-prod",pod=~"fleet-state-.*"}[5m]))

# Clock skew: lag is measured across hosts (fleet-ingest stamps received_at), so skew looks like lag
max(abs(node_timex_offset_seconds))
```

```bash
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=fleet-state
kubectl -n tasuki-prod logs deploy/fleet-state --tail=200 | jq -c 'select(.level != "INFO")'
```

## Mitigate

1. **Recent release?** fleet-state is a plain Deployment, deployed by Argo CD: roll back in git by pinning the
   previous `image.tag` in `deploy/apps/overlays/prod/values/fleet-state.yaml` (create the file if it does not
   exist). `kubectl rollout undo` is reverted by Argo CD's self-heal within seconds. A fleet-ingest release can also slow the path; it is an Argo Rollout
   (`kubectl argo rollouts get rollout fleet-ingest -n tasuki-prod`, abort during its canary).
2. **Consumer lag:** see [kafka-consumer-lag](kafka-consumer-lag.md). Add replicas through git (`replicas:` in the
   same values file), up to 12, one per partition. More replicas than that sit idle.
3. **Redis slow or lost:** fleet-state rebuilds Redis by replaying the last 5 minutes of locations (design §7.2). Lag
   spikes during the rebuild and recovers within seconds. If it does not (`FleetCacheUnavailable`), follow
   [redis-lost](redis-lost.md). Valkey runs with `maxmemory 200mb` and `noeviction`, so a full cache fails writes
   loudly instead of evicting positions: look for write errors in the fleet-state logs.
4. **Clock skew over ~0.5 s:** fix NTP on the affected nodes (or drain them). Fixing the SLI is not the answer.

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

A postmortem is required if one incident burned more than 20 % of the budget
([error-budget policy](../slo/error-budget-policy.md)).
