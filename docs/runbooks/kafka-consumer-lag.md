# Runbook: Kafka consumer lag (fleet-state on courier.locations)

Alert: `KafkaConsumerLagHigh` (`severity: page`): the `fleet-state` consumer group has been more than 7,500 messages
behind on `courier.locations` (about 10 s of peak GPS traffic) for 5 minutes · [rule](../../observability/alerts/kafka.yaml)

## Symptoms

fleet-state is not keeping up with the GPS stream, so courier positions in Redis are getting old. Tracking is not
covered here: it reads the topic without a consumer group. A stalled tracking replica raises
[tracking-feed-stalled](tracking-feed-stalled.md) instead.

## Impact

Dispatch matches orders against stale positions. This alert pages although it is a cause: a consumer that has
*stopped* records no freshness events, so the location freshness SLO goes quiet instead of burning, and lag is the
only signal left.

## Confirm

```promql
# Lag, and whether it is still growing
sum(kafka_consumergroup_lag{consumergroup="fleet-state",topic="courier.locations"})
deriv(sum(kafka_consumergroup_lag{consumergroup="fleet-state",topic="courier.locations"})[10m:1m])

# Consuming at all? Compare the group's consume rate with the topic's produce rate
sum(rate(kafka_consumergroup_current_offset{consumergroup="fleet-state",topic="courier.locations"}[5m]))
sum(rate(kafka_topic_partition_current_offset{topic="courier.locations"}[5m]))

# Skew: one partition far behind points to one stuck consumer
kafka_consumergroup_lag{consumergroup="fleet-state",topic="courier.locations"}

# What fleet-state itself sees
sum(rate(fleet_state_points_total{namespace="tasuki-prod"}[5m]))
```

```bash
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=fleet-state
kubectl -n tasuki-prod logs deploy/fleet-state --tail=200 | jq -c 'select(.msg | test("rebalanc|revoked|assigned"; "i"))'
# Pod name assumes Strimzi's <cluster>-kafka-<n> naming
kubectl -n kafka exec -it tasuki-kafka-0 -- bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --describe --group fleet-state
```

## Mitigate

1. **Rebalance storm** (pods restarting or failing readiness): stop the restarts first (bad rollout → `kubectl argo
   rollouts undo fleet-state -n tasuki-prod`; OOMKilled → raise memory). Lag clears quickly once the group is stable.
2. **Too slow:** scale out, up to 12 replicas (one per partition; more sit idle). If Redis is the bottleneck, check
   Redis latency before adding consumers.
3. **Far behind and Redis badly stale:** catching up is still right, because fleet-state also writes the location
   history to S3. Do not reset the group's offsets to latest without recording the gap in history.
4. **Produce rate far above normal** (a buggy courier app, or a rain surge): check fleet-ingest. It sheds load with
   `503 Retry-After` when Kafka is struggling (`fleet_ingest_shed_requests_total`).

## Escalate

Lag still growing after 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Check the location freshness budget once lag is back to zero; a postmortem is required above 20 %.
