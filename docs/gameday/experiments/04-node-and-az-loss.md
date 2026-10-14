# GD-04: Node loss and AZ impairment

| | |
|---|---|
| Status | **Planned, not run** |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "One AZ lost"; §12 (3 AZs, Karpenter) |
| SLOs at stake | all, briefly: every service runs 2 replicas spread across zones in prod |
| Environments | Variant A (one node): `tasuki-dev`, but nodes are shared, so see the prod guard. Variant B (one AZ): `tasuki-prod` |
| Time box | A: 30 minutes. B: 60 minutes |
| Scripts | `node-loss.sh`, `watch-slis.sh`, `record-timeline.sh` |

## Hypothesis

**Variant A, one node is terminated without warning** (`--mode terminate-instance`, like a spot interruption that
missed its notice):

1. Pods on the node are rescheduled. Karpenter launches a replacement node (spot first) if the rest of the cluster
   lacks room. In prod every service keeps at least one Ready replica, because the chart spreads replicas across zones
   (`maxSkew: 1`, `DoNotSchedule`) and across hosts. Users see only brief retries.
2. In dev (1 replica), each service on the node is down until its pod is rescheduled and Ready: about a minute
   (tracking longer, because it replays its topics before it is ready).
3. A Kafka broker or PostgreSQL instance on the node stays Pending until a node exists **in the same AZ** (EBS volumes
   are zonal). Karpenter should launch one there.

**Variant B, one AZ is drained** (`--zone <az> --mode drain`):

4. PDBs (`maxUnavailable: 1`) keep each service at one Ready replica or more during the drain. Evicted pods land in
   the other two zones.
5. Kafka (prod: 3 brokers, RF 3, min ISR 2) keeps accepting writes with one broker down. The broker in the drained AZ
   stays Pending (its volume is zonal), or comes back on a new node in the same AZ.
6. PostgreSQL: CloudNativePG's primary PodDisruptionBudget blocks evicting the primary until a switchover. If the
   primary is in the drained AZ, the drain waits (or CloudNativePG switches over first): orders creation sees at most
   a failover-sized blip ([GD-06](06-postgres-failover.md)).
7. **What a cordon does not simulate:** cordoned nodes still count as topology domains, and Karpenter may launch a
   fresh node into the "lost" AZ. Both outcomes are observations to record, not failures of the drill. A real AZ
   impairment needs AWS Fault Injection Service (not set up, see gaps).

## Steady state

- **Dashboards:** Tasuki overview, Tasuki service RED, Tasuki SLOs.
- **`watch-slis.sh`** with:

  ```bash
  --extra 'pods pending=sum(kube_pod_status_phase{namespace="{ns}",phase="Pending"})' \
  --extra 'deployments below desired=count(kube_deployment_status_replicas_available{namespace="{ns}"} < kube_deployment_spec_replicas{namespace="{ns}"}) or vector(0)' \
  --extra 'kafka under-replicated partitions=sum(kafka_topic_partition_under_replicated_partition{namespace="{ns}"})'
  ```

- **Placement:** `kubectl --context tasuki get nodes -L karpenter.sh/capacity-type,topology.kubernetes.io/zone` and
  `kubectl --context tasuki -n tasuki-prod get pods -o wide`. Note which zone holds the PostgreSQL primary
  (`kubectl -n tasuki-prod get cluster orders-db -o jsonpath='{.status.currentPrimary}'`) and each Kafka broker.
- **Where Prometheus runs:** `kubectl -n monitoring get pods -o wide`. If the target node holds
  `prometheus-kube-prometheus-stack-prometheus-0`, pick another node, or accept a gap in the metrics.
  `node-loss.sh` warns about this.

## Blast radius

Nodes are **shared by both environments and by monitoring**. `node-loss.sh` lists every pod on the target nodes and
refuses if any belongs to `tasuki-prod` unless the drill is declared as prod (`--namespace tasuki-prod`,
`ALLOW_PROD=yes`). System nodes (`node-pool=system`: Argo CD, Karpenter) are never targeted.

## Preconditions

- Preflight GO in the target namespace (and in `tasuki-prod` for B, budget above 50 % on every SLO).
- Simulator running (`small` in dev; `normal-weekday` at a meal peak in prod).
- At least 3 Karpenter nodes in at least 2 zones, so a drain has somewhere to go.
- For `terminate-instance`: `AWS_PROFILE` and `TASUKI_ACCOUNT_ID` set (the same guard as `make aws-*`).

## Steps

```bash
CTX=tasuki NS=tasuki-dev        # variant B: NS=tasuki-prod, export ALLOW_PROD=yes
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context $CTX --namespace $NS \
  --experiment gd-04-node-az-loss --print-path)"
scripts/gameday/preflight.sh --context $CTX --namespace $NS
scripts/gameday/record-timeline.sh mark steady-state; scripts/gameday/record-timeline.sh snapshot
```

**Variant A, one node**

1. Pick a Karpenter node that runs some `tasuki-dev` pods and, ideally, no `tasuki-prod` pods
   (`kubectl get pods -A -o wide --field-selector spec.nodeName=<node>`).
2. `scripts/gameday/node-loss.sh --context $CTX --namespace $NS --node <node> --mode terminate-instance`
   (or `--mode delete-nodeclaim` for the graceful version, where Karpenter cordons and drains first).
3. Watch `kubectl get nodes -w`, `kubectl get nodeclaims -w`, `kubectl -n $NS get pods -o wide -w`. Note when the node
   goes NotReady and disappears, when replacement pods are Pending, when a NodeClaim is created, and when every
   Deployment is back at its desired count.

**Variant B, one AZ (prod)**

4. Choose the zone. Prefer one that does **not** hold the PostgreSQL primary for the first run; a second run with the
   primary in that zone tests hypothesis 6.
5. `scripts/gameday/node-loss.sh --context $CTX --namespace tasuki-prod --zone <az> --mode drain --timeout 900`.
   The script cordons every Karpenter node in the zone, then drains them one at a time, respecting PDBs.
6. Checkpoints: during each drain, every Deployment keeps one or more available replicas; Kafka under-replicated
   partitions are above 0 while a broker is away and 0 after; orders creation keeps succeeding.
7. Note where evicted pods landed (`-o wide`) and whether Karpenter launched a node in the cordoned zone.
8. `scripts/gameday/node-loss.sh --context $CTX --namespace tasuki-prod --rollback` (uncordon), `mark recovered`, `snapshot`.

## Expected observations

| Signal | Expected |
|---|---|
| Prod Deployments | Never 0 available; at most 1 unavailable each (PDB) |
| Dev Deployments on the node | Down for about a minute each |
| Kafka (prod) | Writes continue; under-replicated partitions while a broker is away |
| Broker or PostgreSQL pod from the lost node | Pending until a node exists in its AZ |
| Karpenter | A NodeClaim within a minute or two when pods are Pending |
| Simulator failures | A short spike of `network`/`timeout`, then normal |

## Abort criteria

In addition to the [global ones](../README.md#abort-criteria-all-experiments):

- A prod Deployment reaches 0 available replicas.
- A drain is blocked for more than 15 minutes by something other than the expected PostgreSQL primary PDB.
- Kafka reports offline partitions, or producers fail for more than 1 minute in prod.

## Rollback

`node-loss.sh --rollback` uncordons the nodes it cordoned (it saves the list). Terminated nodes need no undo:
Karpenter replaces capacity, and the NodePool's consolidation removes spare nodes afterwards.

## Alerts and runbooks

Follow [node-az-loss](../../runbooks/node-az-loss.md) during the drill, and note every step that is wrong or slow.

| Alert | Should it fire? | Runbook |
|---|---|---|
| SLO burn alerts (prod) | No, for a single node; a short spike at most | the SLO runbooks |
| `KubeNodeNotReady` (upstream, 15 min) | Only if a terminated node lingers NotReady for 15 minutes; Karpenter normally removes it sooner | [node-az-loss](../../runbooks/node-az-loss.md) |
| `KubePodNotReady` (upstream, 15 min) | Only if a pod stays Pending or unready for 15 minutes, for example a broker waiting for a node in its AZ | [node-az-loss](../../runbooks/node-az-loss.md) |
| `KafkaUnderReplicated` (prod, 15 min) | If a broker stays away for 15 minutes (variant B, or a broker Pending in a drained AZ) | [kafka-unavailable](../../runbooks/kafka-unavailable.md) |
| `OrdersDatabaseFailover` (ticket) | If the drained zone held the PostgreSQL primary and CloudNativePG switched over | [postgres-failover](../../runbooks/postgres-failover.md) |
| `CourierLocationsLost` (prod, page) | If the node running the simulator is lost: its couriers vanish without going offline | [courier-locations-lost](../../runbooks/courier-locations-lost.md) |
| `KafkaConsumerLagHigh` | No | [kafka-consumer-lag](../../runbooks/kafka-consumer-lag.md) |

## Gaps found while planning

- `topologySpreadConstraints` use the default `nodeTaintsPolicy: Ignore`, so cordoned nodes still count as zones. With
  three or more replicas (the HPAs), a pod may stay Pending during an AZ drain because of `DoNotSchedule` on zones.
  Worth confirming in the drill.
- One NAT gateway serves all three AZs (infra/README.md). Losing its AZ cuts egress (ECR pulls, AWS APIs) everywhere,
  which a drain cannot show.
- No AWS Fault Injection Service role or templates in `infra/`, so a real AZ impairment cannot be run.
- In dev, CloudNativePG runs one instance; if its PDB blocks evicting a lone primary, a drain of its node waits until
  `kubectl drain --timeout`, or until Karpenter's `terminationGracePeriod` (1 h) forces it.
- The simulator is a single pod: losing its node is an outage of the traffic, not of the platform, and it pages
  (`CourierLocationsLost`). Pick the target node knowing where the simulator runs, or treat that page as expected.

## Results

<!-- fill in after the drill -->

| | |
|---|---|
| Date and window (JST) | <!-- fill in after the drill --> |
| Variant, mode, target node or zone | <!-- fill in after the drill --> |
| Driver / observer / scribe | <!-- fill in after the drill --> |
| Drill log | <!-- link --> |
| Hypothesis held? | <!-- per numbered point --> |

| Signal | Expected | Observed |
|---|---|---|
| Time to replacement node (s) | ~60–120 | <!-- fill in after the drill --> |
| Time until every Deployment was back at desired (s) | | <!-- fill in after the drill --> |
| Lowest available replicas, any prod Deployment | ≥ 1 | <!-- fill in after the drill --> |
| Kafka under-replicated partitions, max | > 0 while a broker is away | <!-- fill in after the drill --> |
| PostgreSQL primary moved? | only if its AZ was drained | <!-- fill in after the drill --> |
| Karpenter launched into the cordoned AZ? | possible | <!-- fill in after the drill --> |
| Alerts that fired | | <!-- fill in after the drill --> |
| Budget remaining before / after (prod) | | <!-- fill in after the drill --> |

Surprises: <!-- fill in after the drill -->

Action items: <!-- fill in after the drill -->

Postmortem: <!-- link, or "not required" with the reason -->
