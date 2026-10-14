# Runbook: demand surge, scaling and brownout

No alert of its own: this is the procedure for a surge (design §10, "Rain surge (×3 demand)"). You get here from
`DispatchAssignmentSpeedBudgetBurn` when the cause is supply
([slo-dispatch-assignment-speed](slo-dispatch-assignment-speed.md)), `FleetIngestShedding` with reason `buffer_full`
([courier-locations-lost](courier-locations-lost.md)), `TrackingSSEDropsHigh` ([tracking-sse-drops](tracking-sse-drops.md)),
or kube-prometheus-stack's `KubeHpaMaxedOut`. Game day [GD-02](../gameday/experiments/02-rainy-friday-surge.md).

## Symptoms

Orders arrive faster than couriers can take them: waiting orders pile up per ward, more orders end with
`NoCourierAvailable`, and quotes get longer ETAs (dispatch reports `supply: low`). The location path carries more GPS
points (couriers on jobs report every 4 s instead of every 15 s) and tracking has more viewers.

## Impact

Design §4: a surge is a capacity problem, not a throughput problem. The order API should stay fast and available;
dispatch speed burns because of supply, and that burn is real (it stays in the budget). What must not happen is the
software becoming the bottleneck on top: shedding, dropped tracking events, Pending pods.

## Confirm

```promql
# Demand and supply
sum(rate(orders_created_total{namespace="tasuki-prod"}[10m])) * 3600                     # orders per hour
max by (state) (dispatch_couriers{namespace="tasuki-prod"})                               # available / online
sort_desc(sum by (ward) (dispatch_pending_orders{namespace="tasuki-prod"}))
sum(rate(dispatch_assignment_duration_seconds_count{namespace="tasuki-prod",outcome="no_courier"}[10m]))
/ sum(rate(dispatch_assignment_duration_seconds_count{namespace="tasuki-prod"}[10m]))

# Is the software keeping up?
sum by (reason) (rate(fleet_ingest_shed_requests_total{namespace="tasuki-prod"}[5m]))
sum(rate(tracking_sse_dropped_events_total{namespace="tasuki-prod"}[5m]))
max by (horizontalpodautoscaler) (kube_horizontalpodautoscaler_status_current_replicas{namespace="tasuki-prod"})
max by (horizontalpodautoscaler) (kube_horizontalpodautoscaler_spec_max_replicas{namespace="tasuki-prod"})
sum(kube_pod_status_phase{namespace="tasuki-prod",phase="Pending"})
```

```bash
kubectl -n tasuki-prod get hpa                      # fleet-ingest 2-6 at 70 % CPU, tracking 2-6 at 60 %
kubectl get nodeclaims -o wide                      # Karpenter adding nodes (spot first)?
kubectl -n tasuki-prod describe resourcequota tasuki   # 8 CPU requested in prod caps every scale-out
```

## Mitigate

Design §10's order of priority when something has to give: **orders > dispatch > tracking > history**.

1. **Let quoting be honest.** Nothing to do: dispatch's `GET /v1/eta` answers with longer pickup times (from the
   nearest free courier, or from the edge of the search radius when there is none) and `supply: low` or `none`, and
   orders puts those times into the quote's ETA. P1 has no surge fee: the delivery fee is distance-based. Tell the
   dispatcher on duty; more couriers is the only real fix for supply.
2. **Let autoscaling work.** fleet-ingest and tracking have HPAs; Karpenter adds nodes (spot, then on-demand) when
   pods are Pending, up to the NodePool limit (32 vCPU). If an HPA sits at its maximum while CPU stays high, raise
   `autoscaling.maxReplicas` in `deploy/apps/overlays/prod/values/<service>.yaml`. orders and dispatch run a fixed
   2 replicas: raise `replicas:` there. The prod ResourceQuota (8 CPU of requests) and the NodePool limit cap all of
   it; raising them is a cost decision. All of these are pull requests: Argo CD reverts `kubectl scale` and
   `kubectl edit`.
3. **Brownout tracking** when tracking is CPU-bound or dropping events: set `TRACKING_MIN_INTERVAL: 10s` under `env:`
   in `deploy/apps/overlays/prod/values/tracking.yaml` and merge. Each viewer then gets at most one position per
   courier every 10 s; the age of what is sent (the freshness SLI) does not change. It is an environment variable,
   so it rolls tracking out as a new revision: tracking is an Argo Rollout, so the canary steps (25 %, 50 %, 5 minutes
   each) and the gate run first, and every new pod replays its topics before it is ready. In a hurry, once the
   canary pod is ready, `kubectl argo rollouts promote tracking -n tasuki-prod --full` skips the remaining steps.
4. **What sheds by itself:** fleet-ingest answers `503 Retry-After` when Kafka cannot keep up (couriers send a fresher
   point later); tracking refuses new streams above 5,000 per replica (`503 Retry-After`) and drops events for slow
   viewers; location history is best effort. Orders is never shed on purpose (there is no per-tenant rate limit yet).
5. **After the surge:** revert the brownout pull request (another tracking rollout) and any temporary limits. HPAs
   and Karpenter scale back down by themselves (tracking waits 10 minutes before removing a replica, because every
   removed replica reconnects its viewers at once).

## Escalate

Orders 5xx or latency burning during a surge means the software found a limit: follow
[slo-orders-availability](slo-orders-availability.md) and escalate to the owner in
[CODEOWNERS](../../.github/CODEOWNERS). Supply problems go to the dispatcher on duty.

## Afterwards

Record the peak demand, the maximum replicas and nodes reached, and whether the brownout was needed. If an HPA or
quota was the limit, that is a capacity ticket with the numbers in it.
