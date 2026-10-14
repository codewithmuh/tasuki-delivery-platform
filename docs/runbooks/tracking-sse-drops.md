# Runbook: tracking dropping position events

Alert: `TrackingSSEDropsHigh` (`severity: ticket`): over 2 % of position events have been dropped for slow viewers for
15 minutes · [rule](../../observability/alerts/tracking.yaml)

## Symptoms

Tracking skips an event when a viewer's send buffer is full (a slow client, or a slow tracking pod). The courier on
the map jumps or freezes.

## Impact

It is usually mild: only the newest position matters, so one dropped point is harmless if the next one arrives. A
rising drop rate means viewers see gaps. The tracking freshness SLI only measures events that were *sent*, so it
cannot see drops, which is why this alert exists.

## Confirm

```promql
# Drop ratio; sent events are counted by the freshness histogram
sum(rate(tracking_sse_dropped_events_total{namespace="tasuki-prod"}[10m]))
/ (sum(rate(tracking_event_age_seconds_count{namespace="tasuki-prod"}[10m])) + sum(rate(tracking_sse_dropped_events_total{namespace="tasuki-prod"}[10m])))

# Which replica, and which stream: track (customers) or fleet (ops console)
sum by (pod, stream) (rate(tracking_sse_dropped_events_total{namespace="tasuki-prod"}[10m]))
sum by (pod, stream) (tracking_sse_connections{namespace="tasuki-prod"})

# Saturation of the tracking pods
sum by (pod) (rate(container_cpu_cfs_throttled_periods_total{namespace="tasuki-prod",pod=~"tracking-.*"}[5m]))
```

```bash
kubectl -n tasuki-prod top pods -l app.kubernetes.io/name=tracking,app.kubernetes.io/component=server
kubectl -n tasuki-prod get hpa tracking
# Drops are counted, not logged. The access log shows who streams what (route pattern only, never the token);
# a stream's line is written when it ends, with its duration
kubectl -n tasuki-prod logs -l app.kubernetes.io/name=tracking,app.kubernetes.io/component=server --tail=500 \
  | jq -c 'select(.route == "GET /v1/fleet/stream")'
```

## Mitigate

1. **All pods dropping, CPU high:** the HPA (2–6 replicas) should be scaling tracking out; if it is at its maximum,
   or during a surge, follow [surge-brownout](surge-brownout.md) (raise `maxReplicas`, or the 10 s brownout).
2. **One pod dropping:** delete it. Its viewers reconnect to the others.
3. **A few heavy clients:** an ops console streaming every ward (`/v1/fleet/stream` with no `ward`) receives the whole
   fleet. `tracking_sse_connections{stream="fleet"}` per pod shows how many are open (the access log records the
   route, not the `ward` filter); ask the dispatchers to filter by ward.

## Escalate

Ticket: fix within the week. If the tracking freshness SLO starts burning too, follow
[slo-tracking-freshness](slo-tracking-freshness.md).

## Afterwards

If drops became normal under ordinary load, the per-viewer buffer or the replica count needs a change. Record the
numbers in the ticket.
