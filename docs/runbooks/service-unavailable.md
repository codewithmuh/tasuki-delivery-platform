# Runbook: service unavailable

No alert links this page directly. It is the general procedure behind the SLO burn alerts and kube-prometheus-stack's
`KubePodNotReady` / `KubePodCrashLooping` for a Tasuki service.

## Symptoms

- `/readyz` returns 503 or times out; the Service has no ready endpoints, or fewer than usual.
- Error rate or latency alert fired for the service.

## Impact

Clients get errors or slow responses. Check the SLO dashboard for how much error budget is being burned.

What readiness means differs per service (runtime contract: liveness never checks dependencies, readiness may):

| Service | Unready when |
|---|---|
| orders | Shutting down, or PostgreSQL has failed every check for `READINESS_DB_GRACE_SECONDS` (120 s), or the process has never reached it. During the grace, requests get `503 database_unavailable` + `Retry-After` instead |
| fleet-ingest | Only shutting down: it ignores Kafka on purpose and sheds with `503 Retry-After` |
| fleet-state | Redis unreachable, or rebuilding Redis from Kafka |
| dispatch | Redis unreachable, or paused after a Redis loss until its state is rebuilt |
| tracking | Still replaying `orders.events` and `dispatch.events` after a start (tens of seconds) |

## Confirm

```bash
# Every Tasuki pod carries app.kubernetes.io/name=<service>; the long-running ones also component=server
kubectl -n tasuki-prod get pods -l app.kubernetes.io/name=<service>,app.kubernetes.io/component=server
kubectl -n tasuki-prod describe pod <pod>                    # Events, Last State (OOMKilled?), probe failures
# orders, fleet-ingest, tracking and dispatch are Argo Rollouts in prod: select their logs by label.
# In dev, and for the other services, `kubectl logs deploy/<service>` works too.
kubectl -n tasuki-prod logs -l app.kubernetes.io/name=<service>,app.kubernetes.io/component=server --tail=100
kubectl argo rollouts get rollout <service> -n tasuki-prod   # Rollouts: which revision is serving, any canary
```

The JSON logs carry `request_id`. orders writes lowercase levels (`error`), the Go services uppercase (`ERROR`).

## Mitigate

1. **Recent deploy?** Roll back first, investigate second. During a canary, `kubectl argo rollouts abort <service>
   -n tasuki-prod`. Otherwise pin the previous `image.tag` in `deploy/apps/overlays/<env>/values/<service>.yaml` and
   merge: Argo CD self-heals, so `kubectl rollout undo` and `kubectl argo rollouts undo` are reverted within seconds.
   If a fix cannot wait for a merge, pause self-heal for that one Application first (`argocd app set <service>-prod
   --sync-policy none`), and turn it back on once git matches (`--sync-policy automated --self-heal --auto-prune`).
2. **Crash loop or not starting?** Check the last log lines and `kubectl describe pod` (OOMKilled, failing probe,
   bad config, `CreateContainerConfigError` for a missing Secret key). See
   [broken-secret-stuck-rollout](broken-secret-stuck-rollout.md).
3. **Dependency down?** Readiness fails by design for the dependencies in the table above; fix or fail over the
   dependency ([postgres-failover](postgres-failover.md), [redis-lost](redis-lost.md),
   [kafka-unavailable](kafka-unavailable.md)).
4. **Overloaded?** Scale out through git (`replicas:`, or the HPA's `maxReplicas` where there is one), then look for
   the expensive endpoint in the latency metrics. `kubectl scale` is reverted by Argo CD.

## Escalate

If not mitigated within 30 minutes, escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Write a blameless postmortem: timeline, impact, root cause, what went well, action items with owners.
