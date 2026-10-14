# Runbook: Kafka unavailable or under-replicated

Alerts · [rules](../../observability/alerts/kafka.yaml) · game day [GD-05](../gameday/experiments/05-kafka-broker-loss.md):

- `KafkaUnavailable` (`severity: page`): fewer than 2 brokers ready in `tasuki-prod` for 2 minutes. Writes need
  2 in-sync replicas (`min.insync.replicas` 2), and the KRaft quorum needs 2 of the 3 nodes.
- `KafkaUnderReplicated` (`severity: ticket`): partitions under-replicated for 15 minutes. One broker is away; writes
  still succeed, but the next loss stops them.

While `KafkaUnavailable` fires, the Alertmanager mutes `FleetIngestShedding`, `OrdersOutboxLagHigh` and
`KafkaConsumerLagHigh` in the same namespace: they are consequences.

## Symptoms

Kafka is Strimzi in KRaft mode, one cluster per environment namespace: Kafka `tasuki`, node pool `dual-role` (every
node is a controller and a broker), pods `tasuki-dual-role-<n>`, bootstrap `tasuki-kafka-bootstrap:9092`. Prod has
3 nodes, one per AZ, replication factor 3. Dev has one node, so a dev broker restart is a full outage. Each broker
keeps its data on a zonal EBS volume (`deleteClaim: false` in prod).

## Impact

What design §10 promises when Kafka is unavailable, and what to expect:

| Component | Behaviour |
|---|---|
| orders API | Unaffected: orders and their outbox rows go into PostgreSQL in one transaction |
| outbox relay | Waits and retries; `OrdersOutboxLagHigh` would page after 6 minutes. Nothing is lost |
| fleet-ingest | Sheds with `503 Retry-After: 5`, stays Ready on purpose; GPS points are dropped |
| fleet-state | No new points; couriers leave the geo sets after 60 s (`CourierLocationsLost`) |
| tracking | Viewers see a frozen map (pings only) |
| dispatch | No new orders; its own events wait in its Redis outbox and are resent |

## Confirm

```promql
# Ready brokers (what the page counts), and the exporter's view while any broker answers
sum(kube_pod_status_ready{namespace="tasuki-prod",pod=~"tasuki-dual-role-[0-9]+",condition="true"})
max(kafka_brokers{namespace="tasuki-prod"})
sum by (topic) (kafka_topic_partition_under_replicated_partition{namespace="tasuki-prod"})
count by (topic) (kafka_topic_partition_in_sync_replica{namespace="tasuki-prod"} < 2)        # partitions refusing writes

# The consequences
sum by (reason) (rate(fleet_ingest_shed_requests_total{namespace="tasuki-prod"}[5m]))
max(orders_outbox_lag_seconds{namespace="tasuki-prod"})
```

```bash
kubectl -n tasuki-prod get kafka tasuki                         # Ready condition and its message
kubectl -n tasuki-prod get kafkanodepool dual-role
kubectl -n tasuki-prod get pods -l strimzi.io/cluster=tasuki,strimzi.io/pool-name=dual-role -o wide
kubectl -n tasuki-prod describe pod tasuki-dual-role-<n> | sed -n '/Conditions/,$p'   # Pending? OOMKilled?
kubectl -n tasuki-prod get pvc -l strimzi.io/cluster=tasuki -o wide                   # which volume, which zone
kubectl get nodes -L topology.kubernetes.io/zone,karpenter.sh/capacity-type
kubectl -n tasuki-prod logs tasuki-dual-role-<n> -c kafka --tail=200
kubectl -n strimzi logs deploy/strimzi-cluster-operator --tail=200                    # what the operator is doing
```

## Mitigate

1. **A broker is Pending.** Its EBS volume is zonal, so it can only run on a node in the volume's AZ
   (`describe` shows `volume node affinity conflict`). Karpenter should launch a node there; if it does not, follow
   [node-az-loss](node-az-loss.md) (NodePool CPU limit, an impaired AZ). Never delete a broker's PVC to get it
   scheduled elsewhere: in prod the other two brokers have the data, but a new empty broker must then copy every
   partition, and in dev the PVC *is* the data.
2. **A broker is crash-looping.** Read its last log lines: out of memory (the container limit is 1 Gi, heap 512 Mi),
   a full volume (10 Gi; check the retention of `courier.locations`, 24 h), or a KRaft metadata error. Fix the
   cause in `deploy/apps/base/kafka.yaml` or the prod overlay through a pull request; Strimzi rolls the brokers one
   at a time.
3. **Two brokers lost in prod (quorum lost).** Bring at least one back; nothing else restores writes. Do not lower
   `min.insync.replicas` or the replication factor during the incident: without a controller quorum the cluster
   cannot apply the change, and afterwards it would trade away the durability the design relies on (that would be a
   decision for an ADR, not for an on-call).
4. **Strimzi itself stuck** (the operator log shows a reconciliation that never finishes): restart the operator
   (`kubectl -n strimzi rollout restart deployment/strimzi-cluster-operator`). It reconciles from the Kafka
   resources; restarting it does not touch running brokers.
5. **`KafkaUnderReplicated` only:** find the missing broker (steps 1–2) and bring it back; the replicas catch up by
   themselves.

When the brokers are back, check that the backlogs drain: outbox lag back to 0 within a minute
([orders-outbox-lag](orders-outbox-lag.md)), consumer lag back to normal ([kafka-consumer-lag](kafka-consumer-lag.md)),
shed rate 0, couriers back in the fleet.

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Check that no outbox row was lost (`select count(*) from messaging_outboxevent where published_at is null;` is 0,
see [orders-outbox-lag](orders-outbox-lag.md) for how to run it), and the location freshness and dispatch speed
budgets. GPS points shed during the outage are gone by design; note how many (`fleet_ingest_points_total{result="shed"}`).
