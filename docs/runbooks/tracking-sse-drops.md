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
kubectl -n tasuki-prod top pods -l app.kubernetes.io/name=tracking
kubectl -n tasuki-prod logs deploy/tracking --tail=300 | jq -c 'select(.msg | test("drop|slow"; "i"))'
```

## Mitigate

1. **All pods dropping, CPU high:** scale tracking out, or turn on brownout (10 s updates, design §10) during a surge.
2. **One pod dropping:** delete it. Its viewers reconnect to the others.
3. **A few heavy clients:** an ops console streaming every ward (`/v1/fleet/stream` with no `ward`) receives the whole
   fleet. Check the access logs for that route and ask the dispatcher to filter by ward.

## Escalate

Ticket: fix within the week. If the tracking freshness SLO starts burning too, follow
[slo-tracking-freshness](slo-tracking-freshness.md).

## Afterwards

If drops became normal under ordinary load, the per-viewer buffer or the replica count needs a change. Record the
numbers in the ticket.
