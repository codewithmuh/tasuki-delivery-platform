# Runbook: tracking freshness budget burn

Alert: `TrackingFreshnessBudgetBurn` (`severity: page` or `ticket`) · SLO: 99 % of position events younger than 10 s
when sent to a viewer, over 28 days · [spec](../../observability/slo/specs/tracking.yaml)

## Symptoms

- **page:** more than 13 % of events sent to viewers were 10 s old or older over the last hour and 5 minutes, or
  more than 5.6 % over 6 hours and 30 minutes.
- **ticket:** a slower burn over a day or 3 days.

## Impact

Customers and merchants watch a courier who is not where the map says. Support calls rise ("where is my order?").
The SLI only counts events that were sent. If events are being dropped, see [tracking-sse-drops](tracking-sse-drops.md).

## Confirm

```promql
# Age at send, p99
histogram_quantile(0.99, sum by (le) (rate(tracking_event_age_seconds_bucket{namespace="tasuki-prod"}[5m])))

# Per tracking replica: each reads the whole stream (no consumer group, so no Kafka lag metric), and one slow
# replica hurts only its own viewers
histogram_quantile(0.99, sum by (le, pod) (rate(tracking_event_age_seconds_bucket{namespace="tasuki-prod"}[5m])))

# Is the age already high when points arrive? (couriers' apps batching late, or fleet-ingest slow)
histogram_quantile(0.99, sum by (le) (rate(fleet_state_freshness_seconds_bucket{namespace="tasuki-prod"}[5m])))
```

```bash
kubectl argo rollouts get rollout tracking -n tasuki-prod      # tracking is an Argo Rollout in prod
kubectl -n tasuki-prod get hpa tracking                         # 2-6 replicas at 60 % CPU
kubectl -n tasuki-prod top pods -l app.kubernetes.io/name=tracking,app.kubernetes.io/component=server
```

## Mitigate

1. **Recent rollout?** `kubectl argo rollouts abort tracking -n tasuki-prod` during a canary; after one, pin the
   previous `image.tag` in `deploy/apps/overlays/prod/values/tracking.yaml` and merge (Argo CD self-heal reverts
   `kubectl argo rollouts undo`).
2. **One replica behind:** delete that pod. Its viewers reconnect to healthy replicas, and the new pod starts from the
   latest positions after replaying `orders.events` and `dispatch.events` (tens of seconds). Only the newest point
   matters for a live map.
3. **All replicas behind:** CPU-bound. The HPA (2–6 replicas at 60 % CPU) should already be adding replicas; if it is
   at 6, raise `autoscaling.maxReplicas` in git, or turn on the brownout (10 s updates, design §10). Both are
   described in [surge-brownout](surge-brownout.md). Each replica reads the whole stream, so scaling spreads the
   viewers, not the Kafka reading.
4. **Age high before tracking even sees it** (fleet-state's freshness is just as bad): the problem is upstream, in the
   courier apps, fleet-ingest or Kafka. Check fleet-ingest's shed rate (`fleet_ingest_shed_requests_total`) and the
   brokers ([kafka-unavailable](kafka-unavailable.md)).
5. **A replica sending nothing at all** is a different alert: [tracking-feed-stalled](tracking-feed-stalled.md).

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

A postmortem is required if one incident burned more than 20 % of the budget
([error-budget policy](../slo/error-budget-policy.md)).
