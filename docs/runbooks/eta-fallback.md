# Runbook: ETAs on the fallback estimate

Alert: `ETAFallbackHigh` (`severity: ticket`): over 10 % of ETAs have come from a fallback estimate for 10 minutes ·
[rule](../../observability/alerts/eta-fallback.yaml)

## Symptoms

The `component` label says which layer fell back:

- `orders`: dispatch's `GET /v1/eta` did not answer in time, so the quote used orders' local estimate.
- `dispatch`: OSRM (the `routing` service) failed, so dispatch used straight-line distance × a Tokyo detour factor.

Quotes and assignments keep working in both cases, but less accurately.

## Impact

Mild at first: quotes widen and ETAs drift. Over hours, it hurts the ETA-accuracy business SLO. On the dispatch side,
it can also pick a courier who is close on the map but far by road (across a river, for example).

## Confirm

```promql
# orders: share of quotes on the local estimate, and why (timeout, error, ...)
sum(rate(orders_quotes_created_total{namespace="tasuki-prod",eta_source="fallback"}[10m]))
/ sum(rate(orders_quotes_created_total{namespace="tasuki-prod"}[10m]))
sum by (reason) (rate(orders_eta_fallbacks_total{namespace="tasuki-prod"}[10m]))

# dispatch: share of ETAs without road times
sum(rate(dispatch_eta_requests_total{namespace="tasuki-prod",source="fallback"}[10m]))
/ sum(rate(dispatch_eta_requests_total{namespace="tasuki-prod"}[10m]))

# Is dispatch's ETA endpoint healthy and fast? (runtime contract metrics)
sum by (status) (rate(http_requests_total{namespace="tasuki-prod",service="dispatch",route="GET /v1/eta"}[5m]))
histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="tasuki-prod",service="dispatch",route="GET /v1/eta"}[5m])))
```

```bash
kubectl -n tasuki-prod get pods -l 'app.kubernetes.io/name in (dispatch,routing)'
kubectl -n tasuki-prod describe pod -l app.kubernetes.io/name=routing | grep -A3 -i 'last state'   # OOMKilled?
kubectl -n tasuki-prod logs deploy/dispatch --tail=200 | jq -c 'select(.msg | test("osrm|fallback"; "i"))'
```

## Mitigate

**`component=orders`** (dispatch not answering):

1. Dispatch down or not deployed: fix or deploy dispatch. Orders keeps quoting on its own estimate meanwhile.
2. Dispatch slow: compare its `GET /v1/eta` p99 with orders' timeout for that call. Fix the slowness rather than
   raising the timeout, because quotes wait on it.

**`component=dispatch`** (OSRM failing):

1. OSRM pods down or OOMKilled: the Kantō graph needs a lot of memory. Check the limit, and that the prepared map
   data is mounted.
2. OSRM slow: scale routing out.
3. A new map extract broke routing: roll back the routing data version.

## Escalate

Ticket: fix within the day, because ETA accuracy degrades for as long as it lasts. If dispatch speed starts burning,
follow [slo-dispatch-assignment-speed](slo-dispatch-assignment-speed.md).

## Afterwards

Note how long the fallback ran. The ETA-accuracy report for that period will look worse, and this explains why.
