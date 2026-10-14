# GD-07: Broken secret

| | |
|---|---|
| Status | **Planned, not run** |
| Design | [§11](../../design.md#11-security-and-privacy) (secrets from AWS Secrets Manager through External Secrets); [deploy/README.md](../../../deploy/README.md#secrets) |
| SLOs at stake | none for variant A; location freshness for variant B |
| Environment | Variant A: `tasuki-dev` (`ExternalSecretSyncFailing` covers dev, as a ticket). Variant B: `tasuki-dev` to see the behaviour; `tasuki-prod`, in a window, to test its pages (`CourierTokensRejected` and `CourierLocationsLost` select prod) |
| Time box | 45 minutes for both variants |
| Scripts | `broken-secret.sh`, `watch-slis.sh`, `record-timeline.sh` |

External Secrets copies the JSON secret `tasuki/<env>/app` into the Kubernetes Secret `tasuki-app`. The
ExternalSecret lists every key explicitly (`data[].remoteRef.property`, [deploy/README.md](../../../deploy/README.md#secrets)),
so a key missing from Secrets Manager fails the sync instead of producing a Secret without it. Services read keys as
environment variables, resolved when a container starts: a bad value does nothing to running pods and breaks the
**next** pod start (a rollout, a reschedule, an OOM restart).

## Hypothesis

**Variant A, a key is removed** (someone saves the secret without `FLEET_STREAM_TOKEN`; only tracking reads it):

1. The forced sync **fails**: the ExternalSecret goes `Ready=False` (`SecretSyncedError`), `tasuki-dev` turns Degraded
   in Argo CD, and **the last good Kubernetes Secret stays in place**, key included.
2. So a tracking restart **succeeds**: new pods start with the last good value. Users see nothing.
3. `ExternalSecretSyncFailing` opens a ticket after 10 minutes (in dev too, routed to the ticket channel), pointing at
   [broken-secret-stuck-rollout](../../runbooks/broken-secret-stuck-rollout.md).
4. Restoring the secret version and forcing a sync turns the ExternalSecret Ready again; the alert resolves.

**Variant B, a key has the wrong value** (a rotation of `COURIER_TOKEN_SECRET` applied to fleet-ingest only):

5. The sync succeeds (the key exists) and the restarted fleet-ingest pods start and become **Ready**: readiness
   cannot know the value is wrong.
6. Every courier token the simulator holds was signed by orders with the old value, so fleet-ingest answers
   `401 invalid_token` to every batch. Couriers stop reporting (`simulator_location_batches_total{result="rejected"}`).
7. **In prod, the pages arrive within about 2–3 minutes:** `CourierTokensRejected` (401s above a fifth of the online
   fleet in 10 minutes) and then `CourierLocationsLost` (couriers swept without going offline). In dev neither fires
   (both select prod): the location path is down with only the dashboards to show it.

## Steady state

- **Dashboards:** Tasuki service RED (tracking, fleet-ingest), Tasuki location pipeline.
- **`watch-slis.sh`** with:

  ```bash
  --extra 'ingest 401/s=sum(rate(http_requests_total{namespace="{ns}",service="fleet-ingest",status="401"}[1m])) or vector(0)' \
  --extra 'sim location batches by result=sum by (result) (rate(simulator_location_batches_total{namespace="{ns}"}[1m]))' \
  --extra 'containers waiting=sum by (reason) (kube_pod_container_status_waiting_reason{namespace="{ns}"})' \
  --extra 'externalsecret not ready=sum(externalsecret_status_condition{namespace="{ns}",condition="Ready",status="False"})'
  ```

- `kubectl --context tasuki -n tasuki-dev get externalsecret tasuki-app` (`SecretSynced`, Ready=True).

## Blast radius

One environment's secret (the script derives `tasuki/<env>/app` from the namespace; each ClusterSecretStore reads only
its own prefix). Variant A should affect nothing: the last good Secret stays. Variant B takes the whole location path
down for as long as it lasts (couriers cannot report), and in prod it spends the location freshness budget and pages.

## Preconditions

- Preflight GO; simulator running (`small` in dev, `normal-weekday` at a meal peak in prod).
- `AWS_PROFILE` and `TASUKI_ACCOUNT_ID` set; the script refuses otherwise (the same guard as `make aws-*`).
- No secret rotation or deploy in progress in that environment.
- In prod, fleet-ingest is an Argo Rollout: `--restart` then uses `kubectl argo rollouts restart` (plugin needed). A
  restart cycles the pods; it is not a new revision, so it does not run the canary steps or the gate.

## Steps

```bash
CTX=tasuki NS=tasuki-dev        # variant B in prod: NS=tasuki-prod, export ALLOW_PROD=yes
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context $CTX --namespace $NS \
  --experiment gd-07-broken-secret --print-path)"
scripts/gameday/preflight.sh --context $CTX --namespace $NS
scripts/gameday/record-timeline.sh mark steady-state; scripts/gameday/record-timeline.sh snapshot
```

**Variant A**

1. `scripts/gameday/broken-secret.sh --context $CTX --namespace $NS --key FLEET_STREAM_TOKEN --mode remove-key --restart tracking`.
   It saves the current version ID, writes a new version without the key, forces the ExternalSecret to sync, reports
   the ExternalSecret's state and whether the Kubernetes Secret still has the key (names only, never values), then
   restarts tracking and shows the pods.
2. Checkpoints: `kubectl -n $NS get externalsecret tasuki-app` (`SecretSyncedError`);
   `kubectl -n argocd get application tasuki-dev` (Degraded); the tracking restart completing; at T+10 min,
   `ExternalSecretSyncFailing` in `ALERTS` and in the Slack ticket channel.
3. Follow [broken-secret-stuck-rollout](../../runbooks/broken-secret-stuck-rollout.md) from the alert as the on-call
   would. Note how long it took to get from the alert to "a key is missing in Secrets Manager".
4. `broken-secret.sh ... --key FLEET_STREAM_TOKEN --restart tracking --rollback`. Expect the ExternalSecret Ready again
   and the alert resolving.

**Variant B**

5. `broken-secret.sh ... --key COURIER_TOKEN_SECRET --mode wrong-value --restart fleet-ingest`.
6. Checkpoints: fleet-ingest Ready; 401s rising; simulator batches `rejected`; accepted points falling to 0. In prod,
   note when `CourierTokensRejected` and `CourierLocationsLost` fire (about 2–3 minutes expected) and follow
   [courier-locations-lost](../../runbooks/courier-locations-lost.md).
7. `broken-secret.sh ... --key COURIER_TOKEN_SECRET --restart fleet-ingest --rollback`. Accepted points recover.
8. `mark recovered`, `snapshot`, `mark end`.

## Expected observations

| Signal | Variant A | Variant B |
|---|---|---|
| ExternalSecret | `Ready=False`, `SecretSyncedError` | `Ready=True` |
| Kubernetes Secret `tasuki-app` | Unchanged (last good, key present) | Has the wrong value |
| Restarted pods | Start normally | Start and are Ready |
| User impact | None | Couriers rejected: location path down |
| Alerts | `ExternalSecretSyncFailing` (ticket) after 10 minutes | Prod: `CourierTokensRejected`, then `CourierLocationsLost` (pages) within ~2–3 minutes. Dev: none |

## Abort criteria

In addition to the [global ones](../README.md#abort-criteria-all-experiments):

- Any service other than the restarted one goes down.
- Variant A: the Kubernetes Secret loses the key anyway (the explicit-keys protection did not hold). Roll back at once.
- `--rollback` fails to move `AWSCURRENT` back. Then restore by hand:
  `aws secretsmanager update-secret-version-stage --secret-id tasuki/<env>/app --version-stage AWSCURRENT --move-to-version-id <saved> --remove-from-version-id <bad>`;
  the version IDs are in the script's state file and in the drill log.

## Rollback

`broken-secret.sh --rollback` moves `AWSCURRENT` back to the saved version, forces a sync, and restarts the named
workload. The bad version stays in Secrets Manager as a non-current version (harmless).

## Alerts and runbooks

| Alert | Should it fire? | Runbook |
|---|---|---|
| `ExternalSecretSyncFailing` (ticket, 10 min; dev and prod) | **Yes**, variant A | [broken-secret-stuck-rollout](../../runbooks/broken-secret-stuck-rollout.md) |
| `CourierTokensRejected` (page, prod) | **Yes**, variant B in prod | [courier-locations-lost](../../runbooks/courier-locations-lost.md) |
| `CourierLocationsLost` (page, prod) | **Yes**, variant B in prod, shortly after | [courier-locations-lost](../../runbooks/courier-locations-lost.md) |
| `KubeContainerWaiting`, `KubeDeploymentRolloutStuck`, `RolloutDegraded` | **No**: thanks to the explicit keys, no pod is left without its key | [broken-secret-stuck-rollout](../../runbooks/broken-secret-stuck-rollout.md) |

## Gaps found while planning

- Rotating `COURIER_TOKEN_SECRET` is not documented. Orders issues tokens and fleet-ingest (and dispatch) verify them, so
  a rotation needs two accepted secrets for a while, as webhooks already do (design §11). Variant B is that rotation
  done wrong.
- In dev, variant B is silent: both location alerts select prod.
- Found earlier and since addressed: `dataFrom.extract` accepting a secret with missing keys, no ExternalSecret alert,
  no runbook for a broken secret or a stuck rollout, no alert on 401s or lost couriers.

## Results

<!-- fill in after the drill -->

| | |
|---|---|
| Date and window (JST) | <!-- fill in after the drill --> |
| Variants run | <!-- fill in after the drill --> |
| Driver / observer / scribe | <!-- fill in after the drill --> |
| Drill log | <!-- link --> |
| Hypothesis held? | <!-- per numbered point --> |

| Signal | Expected | Observed |
|---|---|---|
| A: ExternalSecret state after the sync; key still in `tasuki-app`? | `SecretSyncedError`; yes | <!-- fill in after the drill --> |
| A: tracking restart completed? | yes | <!-- fill in after the drill --> |
| A: `ExternalSecretSyncFailing`, time after the sync | ~10 min | <!-- fill in after the drill --> |
| A: time to diagnose from the alert | | <!-- fill in after the drill --> |
| B: fleet-ingest Ready with the wrong value? | yes | <!-- fill in after the drill --> |
| B: accepted points during the fault | ~0 | <!-- fill in after the drill --> |
| B: first page and its time after the restart (prod) | `CourierTokensRejected`, ~2–3 min | <!-- fill in after the drill --> |
| Time to recover after `--rollback` (s) | | <!-- fill in after the drill --> |

Surprises: <!-- fill in after the drill -->

Action items: <!-- fill in after the drill -->

Postmortem: <!-- link, or "not required" with the reason -->
