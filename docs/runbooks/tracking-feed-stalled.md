# Runbook: tracking replica stalled

Alert: `TrackingFeedStalled` (`severity: page`): a tracking replica holds ops-console streams but has sent no position
events for 5 minutes while fleet-ingest is accepting points · [rule](../../observability/alerts/tracking.yaml)

## Symptoms

The `pod` label names the replica. Every tracking replica reads all of `courier.locations` without a consumer group,
so Kafka has no lag to report for tracking. Instead, the alert uses the ops-console stream, which receives every
courier: if couriers are reporting and a replica with console viewers sends nothing, that replica has stopped reading
or stopped writing.

## Impact

Every customer and console viewer on that replica sees a frozen map. This alert pages because the tracking freshness
SLO only measures events that *are* sent, so a silent replica does not burn it.

## Confirm

```promql
# Events sent per replica and stream; the stalled pod is flat
sum by (pod, stream) (rate(tracking_event_age_seconds_count{namespace="tasuki-prod"}[5m]))
sum by (pod, stream) (tracking_sse_connections{namespace="tasuki-prod"})

# Couriers are reporting (the alert requires it)
sum(rate(fleet_ingest_points_total{namespace="tasuki-prod",result="accepted"}[5m]))

# Records it could not decode (a schema change?)
sum by (pod, topic) (rate(tracking_records_invalid_total{namespace="tasuki-prod"}[5m]))
```

```bash
kubectl -n tasuki-prod logs <pod> --tail=200 | jq -c 'select(.level != "INFO")'
kubectl -n tasuki-prod describe pod <pod> | grep -A5 -i 'conditions\|last state'
```

## Mitigate

1. **Delete the pod.** Its viewers reconnect to the other replicas (SSE clients retry), and the replacement starts from
   recent positions. That restores service within seconds. Keep the logs first: `kubectl logs <pod> > stalled.log`.
2. **Every replica stalls in turn:** the cause is shared, such as broker connectivity, a NetworkPolicy change, or a
   message the decoder cannot handle (`tracking_records_invalid_total` rising). Roll back the last change: during a
   canary `kubectl argo rollouts abort tracking -n tasuki-prod`; after one, pin the previous `image.tag` in
   `deploy/apps/overlays/prod/values/tracking.yaml` and merge (Argo CD self-heal reverts `kubectl argo rollouts
   undo`). A NetworkPolicy change is reverted the same way, in git.

## Escalate

Not resolved by a restart within 15 minutes, or recurring: escalate to the owner in
[CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

A stall that a restart fixes is a bug (a stuck goroutine or a lost connection without a reconnect). Open a ticket
with the saved logs. Consider having tracking export the age of its last consumed record, which would make this alert
simpler and catch replicas with only customer streams.
