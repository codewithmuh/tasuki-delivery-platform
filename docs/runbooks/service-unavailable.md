# Runbook: service unavailable

## Symptoms

- `/readyz` returns 503 or times out; the load balancer marks targets unhealthy.
- Error rate or latency alert fired for the service.

## Impact

Clients get errors or slow responses. Check the SLO dashboard for how much error budget is being burned.

## Confirm

```bash
curl -fsS https://<host>/readyz            # readiness from outside
kubectl get pods -l app=<service>           # or: docker compose ps
kubectl logs deploy/<service> --tail=100    # JSON logs; filter by request_id
```

## Mitigate

1. **Recent deploy?** Roll back first, investigate second.
2. **Crash loop?** Check the last log lines and `kubectl describe pod` (OOMKilled, failing probe, bad config).
3. **Dependency down?** Readiness will fail by design; fix or fail over the dependency.
4. **Overloaded?** Scale out, then look for the expensive endpoint in the latency metrics.

## Escalate

If not mitigated within 30 minutes, escalate to the owner in [CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

Write a blameless postmortem: timeline, impact, root cause, what went well, action items with owners.
