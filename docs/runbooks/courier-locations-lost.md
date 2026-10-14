# Runbook: courier locations lost (rejected tokens, shedding, couriers dropping out)

Alerts · [rules](../../observability/alerts/fleet.yaml) · game days [GD-05](../gameday/experiments/05-kafka-broker-loss.md),
[GD-07](../gameday/experiments/07-broken-secret.md) variant B:

- `CourierLocationsLost` (`severity: page`): in 10 minutes, more couriers dropped out of the live fleet without going
  offline than a fifth of those online in the last 30 minutes (at least 5).
- `CourierTokensRejected` (`severity: page`, label `service`): fleet-ingest or dispatch answered 401 more often in
  10 minutes than a fifth of the couriers online (at least 10 times).
- `FleetIngestShedding` (`severity: ticket`): more than 5 % of GPS points refused with 503 for 15 minutes.

Both pages keep firing for 30 minutes after their evidence (a burst) has passed, so a resolved page means it really
was resolved, not that the couriers gave up.

## Symptoms

Couriers' apps send GPS batches to fleet-ingest (`POST /v1/locations`, courier JWT); fleet-ingest publishes them to
`courier.locations`; fleet-state writes them to Redis, where dispatch finds couriers. A courier that ends a shift
sends a last `offline` point and leaves the geo sets at once. One whose points stop arriving is swept after 60 s.
Sweeps therefore count couriers **lost**, not couriers done for the day.

| Clue | Likely cause |
|---|---|
| 401s from fleet-ingest and/or dispatch | `COURIER_TOKEN_SECRET` differs between orders (signs the tokens) and the verifiers |
| 503s, `fleet_ingest_shed_requests_total` rising | Kafka is down or slow (`timeout`, `kafka_error`), or fleet-ingest is overloaded (`buffer_full`) |
| 202s keep coming but couriers are swept | fleet-state is not writing them: consumer lag or Redis ([kafka-consumer-lag](kafka-consumer-lag.md), [redis-lost](redis-lost.md)) |
| No requests at all | The path to fleet-ingest: its pods, a NetworkPolicy, or the clients themselves |

## Impact

Dispatch cannot offer orders to couriers it cannot see, and their positions freeze on the map. None of this burns the
location freshness SLO: it only judges points that arrive. A secret mismatch also passes readiness, so every pod
stays Ready. Readiness cannot know a value is wrong.

## Confirm

```promql
# Requests by status on the courier-facing services (route includes the method, e.g. "POST /v1/locations")
sum by (service, route, status) (rate(http_requests_total{namespace="tasuki-prod",service=~"fleet-ingest|dispatch"}[5m]))

# Points by result, and why fleet-ingest sheds
sum by (result) (rate(fleet_ingest_points_total{namespace="tasuki-prod"}[5m]))
sum by (reason) (rate(fleet_ingest_shed_requests_total{namespace="tasuki-prod"}[5m]))

# The fleet as dispatch sees it, and couriers swept
max by (state) (dispatch_couriers{namespace="tasuki-prod"})
sum(increase(fleet_state_swept_couriers_total{namespace="tasuki-prod"}[10m]))

# The clients' side (the simulator plays the courier apps): sent, shed, rejected, failed
sum by (result) (rate(simulator_location_batches_total{namespace="tasuki-prod"}[5m]))
```

For 401s, compare when the secret changed with when each side's pods started. Services read `tasuki-app` as
environment variables at container start, so pods started on different sides of a change disagree. Never print the
value; a hash is enough:

```bash
kubectl -n tasuki-prod get externalsecret tasuki-app \
  -o jsonpath='{.status.refreshTime}{"  "}{.status.conditions[?(@.type=="Ready")].reason}{"\n"}'
kubectl -n tasuki-prod get secret tasuki-app -o jsonpath='{.data.COURIER_TOKEN_SECRET}' | base64 -d | sha256sum
kubectl -n tasuki-prod get pods -l 'app.kubernetes.io/name in (orders,fleet-ingest,dispatch)' \
  -o custom-columns=POD:.metadata.name,STARTED:.status.startTime
```

Pods started before the Secret's last change hold the old value. The fleet-ingest log line for a rejected token is
the access log (`status` 401); the simulator logs `courier token rejected; logging off`.

## Mitigate

**Tokens rejected (401):**

1. Decide which value is right, usually the one orders signs with. If the change was a mistake, put the previous
   version back in Secrets Manager (`aws secretsmanager update-secret-version-stage --secret-id tasuki/prod/app
   --version-stage AWSCURRENT --move-to-version-id <previous> --remove-from-version-id <current>`), then force a sync:
   `kubectl -n tasuki-prod annotate externalsecret tasuki-app force-sync="$(date +%s)" --overwrite`.
2. Restart **all three** of orders, fleet-ingest and dispatch, so they read the same value: `kubectl argo rollouts
   restart <service> -n tasuki-prod` (they are Rollouts in prod; a restart is not a new revision, so it runs no canary),
   or delete their pods one at a time.
3. Tokens signed with the other value stay invalid until they expire (24 h). Real courier apps log in again and get a
   new token. The simulator's couriers log off at the first 401 and do not come back on their own: restart the
   simulator (its values file in `deploy/apps/overlays/prod/values/`, or delete its pod).
4. Rotating `COURIER_TOKEN_SECRET` on purpose invalidates every courier token at once, because no service accepts two
   secrets yet (a gap from GD-07). Do it only in a window, restarting the three services together.

**Shedding (503):**

1. `timeout` or `kafka_error`: Kafka is slow or down. Follow [kafka-unavailable](kafka-unavailable.md).
2. `buffer_full`: fleet-ingest cannot keep up with the load. Its HPA (2–6 replicas at 70 % CPU) should be scaling;
   if it is at 6, or Pending pods cannot be placed, see [surge-brownout](surge-brownout.md).

**No requests at all:** check fleet-ingest's pods and Rollout (`kubectl argo rollouts get rollout fleet-ingest -n
tasuki-prod`); a NetworkPolicy change (fleet-ingest admits only the simulator, `networkPolicy.allowFrom` in
`deploy/apps/services/fleet-ingest.yaml`); and the clients (the simulator pod running?). A killed simulator also
looks like every courier vanishing at once (`CourierLocationsLost` fires in a node-loss drill if it ran there).

## Escalate

Page not mitigated within 30 minutes: escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS). A secret
problem also goes to whoever changed `tasuki/prod/app`.

## Afterwards

Record how long couriers were dark and how many orders waited (dispatch speed budget). A secret mismatch deserves a
postmortem even if it was short: it is the outage readiness and the SLOs could not see.
