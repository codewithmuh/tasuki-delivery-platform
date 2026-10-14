# Runbook: broken secret or stuck rollout

Alerts:

- `ExternalSecretSyncFailing` (`severity: ticket`, labels `namespace`, `name`): an ExternalSecret has not been Ready
  for 10 minutes · [rule](../../observability/alerts/platform.yaml)
- `RolloutDegraded` (`severity: ticket`, labels `namespace`, `name`): an Argo Rollout was aborted or stopped
  progressing · [rule](../../observability/alerts/platform.yaml)
- From kube-prometheus-stack, for pods in `tasuki-*` (severity `warning`, routed to the ticket channel; their
  `runbook_url` points upstream, but this page is the Tasuki procedure): `KubePodNotReady` (Pending or unready for
  15 minutes, which includes `CreateContainerConfigError`), `KubePodCrashLooping` (15 minutes),
  `KubeContainerWaiting` (1 hour), `KubeDeploymentRolloutStuck` (dev's Deployments; prod's four Rollouts are covered
  by `RolloutDegraded`).

Game day [GD-07](../gameday/experiments/07-broken-secret.md).

## Symptoms

Secrets come from AWS Secrets Manager (`tasuki/<env>/app`, one JSON secret per environment) through External
Secrets into the Kubernetes Secret `tasuki-app` (refreshed every hour). The ExternalSecret lists every key with
`remoteRef.property`, so a key missing in Secrets Manager **fails the sync** and the last good Secret stays in place.
Services read the keys as environment variables at container start, so a changed value reaches a pod only when it
(re)starts.

| What you see | Usual cause |
|---|---|
| `ExternalSecretSyncFailing` for `tasuki-app` | A key deleted or renamed in `tasuki/<env>/app`; the secret deleted; IAM (Pod Identity for `external-secrets/external-secrets`) broken |
| New pods `CreateContainerConfigError` | The Kubernetes Secret lacks a key the chart references (`secretEnv`), or the Secret is missing |
| New pods `ImagePullBackOff` | The image tag does not exist (or was never signed and pushed) |
| New pods running but never Ready, or crash-looping | Bad config or code: read the logs |
| `RolloutDegraded` with an aborted canary | The error-budget gate said no (its AnalysisRun failed), or someone aborted it |

## Impact

Usually none yet. Rollouts and Deployments keep their old pods until new ones are ready (`maxUnavailable: 0`), and a
failed sync keeps the last good Secret. The danger is what comes next: the next restart, reschedule or node loss
starts pods with the broken configuration, and a rotation never reaches the cluster. The one case that *is* an outage
already: a wrong value with all the right keys, which readiness cannot see. For courier tokens, see
[courier-locations-lost](courier-locations-lost.md).

## Confirm

```bash
NS=tasuki-prod
kubectl -n $NS get externalsecret tasuki-app                     # STATUS SecretSynced / SecretSyncedError
kubectl -n $NS describe externalsecret tasuki-app | sed -n '/Events/,$p'   # names the missing key or the AWS error
kubectl -n external-secrets logs deploy/external-secrets --tail=100 | grep -i error
# Which keys the Kubernetes Secret has (names only, never values)
kubectl -n $NS get secret tasuki-app -o json | jq -r '.data | keys[]'

kubectl argo rollouts get rollout <service> -n $NS               # prod: orders, fleet-ingest, tracking, dispatch
kubectl -n $NS get analysisruns --sort-by=.metadata.creationTimestamp | grep "^<service>-"
kubectl -n $NS rollout status deployment/<service> --timeout=10s # dev, and prod's other services
kubectl -n $NS get pods -l app.kubernetes.io/name=<service> -o wide
kubectl -n $NS describe pod <new pod> | sed -n '/Events/,$p'      # CreateContainerConfigError: couldn't find key ...
```

## Mitigate

1. **Missing or wrong key in Secrets Manager:** restore it. The cleanest undo moves `AWSCURRENT` back to the previous
   version (`aws secretsmanager list-secret-version-ids --secret-id tasuki/<env>/app`, then
   `aws secretsmanager update-secret-version-stage --secret-id tasuki/<env>/app --version-stage AWSCURRENT
   --move-to-version-id <previous> --remove-from-version-id <current>`). Then force a sync:
   `kubectl -n $NS annotate externalsecret tasuki-app force-sync="$(date +%s)" --overwrite`. Never paste a secret
   value into a terminal log, an issue or a chat.
2. **A new key a release needs:** add it to `tasuki/<env>/app` **first**, and to the ExternalSecret's `data:` list in
   `deploy/apps/base/external-secret.yaml` (and the table in `deploy/README.md`), then release the code that reads it.
3. **Pods waiting with `CreateContainerConfigError`:** once the Secret has the key, they start by themselves; the
   rollout continues.
4. **A stuck or Degraded Rollout:**
   - Aborted by the gate (the AnalysisRun for `error-budget` failed): the budget is spent; follow
     [error-budget-exhausted](error-budget-exhausted.md). A failed gate *query* is
     [rollout-gate-budget-unknown](rollout-gate-budget-unknown.md).
   - New pods cannot start or never become ready: fix the cause in git and let Argo CD sync, or roll back by pinning
     the previous `image.tag` in `deploy/apps/overlays/prod/values/<service>.yaml`. Argo CD's self-heal reverts
     `kubectl argo rollouts undo`; `abort` and `retry` are safe (they only touch the Rollout's status).
   - In dev, the same for Deployments: fix or revert in git.
5. **A secret change that must reach running pods** (a rotation): restart the services that read the key, all
   together if they must agree (orders, fleet-ingest and dispatch share `COURIER_TOKEN_SECRET`):
   `kubectl argo rollouts restart <service> -n tasuki-prod` for Rollouts, `kubectl -n $NS rollout restart
   deployment/<service>` for Deployments.

## Escalate

Ticket: fix within the day. If a restart or reschedule already started pods with a broken configuration and users
see errors, it is an incident: follow [service-unavailable](service-unavailable.md).

## Afterwards

If the bad change came through Secrets Manager, record who changed what (CloudTrail) in the ticket. Rotations that need
two values accepted at once (courier and tracking token secrets) are not supported yet; see GD-07's gaps.
