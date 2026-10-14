# Runbook: node or availability-zone loss, pods that cannot be placed

No Tasuki alert of its own: kube-prometheus-stack already covers nodes and Pending pods, with severity `warning`
(routed to the ticket channel) and upstream runbook links. This page is the Tasuki procedure behind them:

| Alert (kube-prometheus-stack) | Fires when |
|---|---|
| `KubeNodeNotReady`, `KubeNodeUnreachable` | A node has been NotReady / unreachable for 15 minutes |
| `KubePodNotReady` | A pod has been Pending or unready for 15 minutes (unschedulable pods included) |
| `KubeDeploymentReplicasMismatch`, `KubeStatefulSetReplicasMismatch` | Fewer ready replicas than desired for 15 minutes |
| `KubeQuotaAlmostFull`, `KubeQuotaFullyUsed` | A ResourceQuota is over 90 % / at 100 % for 15 minutes. Both are `info`, which the routing drops, so look for `exceeded quota` events yourself |

The Tasuki alerts you may see at the same time are consequences, each with its own runbook: `KafkaUnderReplicated`
or `KafkaUnavailable` ([kafka-unavailable](kafka-unavailable.md)), `OrdersDatabaseFailover` or
`OrdersDatabaseReplicationDegraded` ([postgres-failover](postgres-failover.md)), `FleetCacheUnavailable`
([redis-lost](redis-lost.md)). Game day [GD-04](../gameday/experiments/04-node-and-az-loss.md).

## Symptoms

Worker nodes come from Karpenter (NodePool `default`: spot first, then on-demand; at most 32 vCPU and 128 Gi in
total). Both environments share them. In prod every service runs 2 replicas spread across zones (`DoNotSchedule`)
with a PodDisruptionBudget of `maxUnavailable: 1`; Kafka has one broker per AZ (replication factor 3, min in-sync 2);
`orders-db` keeps its standby in another AZ. Dev runs one of everything.

## Impact

- **One node lost:** in prod, brief retries; in dev, each service on the node is down until its pod is rescheduled
  (about a minute; tracking longer, it replays its topics first).
- **One AZ lost (prod):** brief retries, by design. The Kafka broker and the PostgreSQL instance from that AZ stay
  down, because their EBS volumes are zonal: redundancy is gone until the AZ is back.
- **The NAT gateway's AZ lost:** one NAT gateway serves all three AZs (`infra/README.md`), so egress stops everywhere:
  new pods cannot pull images, External Secrets cannot reach Secrets Manager, backups cannot reach S3. Running pods
  inside the cluster keep working.

## Confirm

```bash
kubectl get nodes -L topology.kubernetes.io/zone,karpenter.sh/capacity-type,karpenter.sh/nodepool
kubectl get nodeclaims -o wide                                     # Karpenter's launches, by zone and type
kubectl get pods -A --field-selector=status.phase=Pending -o wide
kubectl -n tasuki-prod describe pod <pending pod> | sed -n '/Events/,$p'   # why it cannot be placed
kubectl -n kube-system logs deploy/karpenter --tail=200 | jq -c 'select(.level != "INFO" or (.message | test("launch|disrupt"; "i")))'
kubectl get nodepool default -o jsonpath='{.status.resources}{"\n"}{.spec.limits}{"\n"}'   # usage against the limit
kubectl -n tasuki-prod describe resourcequota tasuki               # requests.cpu 8 in prod, 4 in dev
kubectl -n tasuki-prod get events --field-selector reason=FailedCreate   # "exceeded quota" stops pod creation
```

```promql
sum by (namespace) (kube_pod_status_phase{namespace=~"tasuki-.*",phase="Pending"})
kube_node_status_condition{condition="Ready",status="true"} == 0          # nodes that are not Ready
sum by (namespace, reason) (kube_pod_container_status_waiting_reason{namespace=~"tasuki-.*"})
```

## Mitigate

Read the `describe` events of a Pending pod first; the reason decides the fix.

1. **`Insufficient cpu/memory` and no new NodeClaim:** Karpenter is at its NodePool limit (32 vCPU) or cannot get
   capacity. Raise `limits` in `deploy/platform/manifests/karpenter/nodepool.yaml` through a pull request (a cost
   decision), or remove load. Karpenter falls back from spot to on-demand by itself.
2. **`exceeded quota` (FailedCreate on the ReplicaSet, no Pending pod at all):** the namespace's ResourceQuota is
   full. Raise it in the overlay (`deploy/apps/overlays/<env>/kustomization.yaml`) or remove load.
3. **`volume node affinity conflict`:** a Kafka broker or PostgreSQL instance whose EBS volume is in another zone.
   It needs a node in that zone; Karpenter launches one unless the zone itself is impaired. Do not delete the PVC to
   move it: prod survives on the other replicas until the zone is back, and in dev the volume is the only copy.
4. **`didn't match pod topology spread constraints`:** replicas must spread across zones (`DoNotSchedule`), and
   with a zone gone or cordoned a third replica (an HPA scaling out) may have nowhere to go. Cordoned nodes still
   count as zones. The running replicas serve; this resolves when the zone is back. Changing the spread is a chart
   change in git, not an incident step.
5. **A drain waits on the PostgreSQL primary:** CloudNativePG's PodDisruptionBudget blocks evicting the primary.
   Switch over first: `kubectl cnpg promote orders-db <standby-pod> -n tasuki-prod`, then the drain continues.
6. **Egress gone (NAT):** nothing to fix in the cluster; restore the NAT gateway in `infra/` or wait for the AZ.
   Expect `ImagePullBackOff` for new pods, `ExternalSecretSyncFailing` and `OrdersDatabaseBackupFailing`.

## Escalate

A prod Deployment or Rollout at 0 available replicas, or the cluster unable to place pods for 30 minutes: escalate to
the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Check that replicas spread across zones again (`kubectl -n tasuki-prod get pods -o wide`), that the Kafka broker and
the PostgreSQL standby from the lost zone are back and caught up, and uncordon any node a drill cordoned
(`scripts/gameday/node-loss.sh --rollback`).
