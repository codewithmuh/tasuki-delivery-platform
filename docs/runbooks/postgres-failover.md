# Runbook: PostgreSQL (orders-db) primary lost, failover, standby degraded

Alerts · [rules](../../observability/alerts/postgres.yaml) · game day [GD-06](../gameday/experiments/06-postgres-failover.md):

- `OrdersDatabaseUnavailable` (`severity: page`): no `orders-db` instance has accepted writes for 2 minutes.
- `OrdersDatabaseFailover` (`severity: ticket`, label `pod`): that pod was a standby less than 15 minutes ago and is
  the primary now.
- `OrdersDatabaseReplicationDegraded` (`severity: ticket`, label `problem`): `no-standby` (the primary has had no
  streaming standby for 10 minutes) or `lag` (the standby has been over 30 s behind for 5 minutes).

## Symptoms

The orders database is the CloudNativePG Cluster `orders-db` (PostgreSQL 18): one instance in dev, a primary and one
**asynchronous** standby in another AZ in prod. Services: `orders-db-rw` (the primary, what orders uses),
`orders-db-ro`, `orders-db-r`. When the primary fails, the operator promotes the standby and moves `orders-db-rw` to
it; design §10 allows 1–2 minutes.

What orders does meanwhile: requests that need the database get `503 database_unavailable` with `Retry-After: 2`,
which clients retry with the same `Idempotency-Key`. After 120 s of failing checks (`READINESS_DB_GRACE_SECONDS`)
the orders pods turn unready, requests fail at the connection, and the availability SLI stops counting them: its
burn alert may resolve while this page goes on.

## Impact

- **Unavailable:** no quotes and no new orders. Dispatch, tracking and couriers keep working on orders already
  confirmed. The outbox relay and the dispatch-event consumer wait and reconnect.
- **Failover:** with asynchronous replication, the transactions the standby had not received when the primary died
  are gone: orders that a client was told were created may not exist. The standby's lag just before the failover
  bounds how much.
- **Degraded:** nothing visible yet. The next primary failure becomes an outage (no standby), or loses more data
  (lag).

## Confirm

```bash
kubectl cnpg status orders-db -n tasuki-prod          # primary, standby, replication, continuous archiving
kubectl -n tasuki-prod get cluster orders-db          # phase, ready instances
kubectl -n tasuki-prod get cluster orders-db -o jsonpath='{.status.currentPrimary}{"\n"}'
kubectl -n tasuki-prod get pods -l cnpg.io/cluster=orders-db -L cnpg.io/instanceRole -o wide   # role and zone
kubectl -n cnpg-system logs deploy/cnpg-cloudnative-pg --since=30m | jq -c 'select(.msg | test("failover|switchover|promot|primary"; "i"))'
kubectl -n tasuki-prod logs <instance> -c postgres --tail=100          # the instance manager and PostgreSQL, JSON
```

```promql
# Which instance is primary (0) or standby (1), and whether PostgreSQL answers at all
cnpg_pg_replication_in_recovery{namespace="tasuki-prod"}
cnpg_collector_up{namespace="tasuki-prod"}
cnpg_pg_replication_streaming_replicas{namespace="tasuki-prod"}       # on the primary: 1 expected
cnpg_pg_replication_lag{namespace="tasuki-prod"}                      # on the standby: seconds behind

# What orders sees, by reason (connection / shutdown / read_only / too_many_connections / timeout)
sum by (reason) (rate(orders_database_unavailable_total{namespace="tasuki-prod"}[5m]))
# What clients see (the simulator retries 503s with the same Idempotency-Key)
sum by (stage, reason) (rate(simulator_orders_failed_total{namespace="tasuki-prod"}[1m]))
```

## Mitigate

1. **Failover in progress** (`currentPrimary` changing, standby promoting): let it finish; it should take well under
   2 minutes. Clients retry safely.
2. **No primary after 2 minutes.** `kubectl cnpg status` usually says why. If the standby is healthy and caught up
   but not promoted, promote it: `kubectl cnpg promote orders-db <standby-pod> -n tasuki-prod`. The promotion accepts
   losing whatever the standby had not received; write down its last lag in the incident. If no instance can start,
   look at the pods: Pending because their zonal EBS volume's AZ has no node ([node-az-loss](node-az-loss.md)), or a
   full volume (5 Gi; `kubectl -n tasuki-prod exec <instance> -c postgres -- df -h /var/lib/postgresql/data`). WAL
   that cannot be archived piles up there ([backup-restore](backup-restore.md)).
3. **The volumes are gone** (both instances lost with their data): restore into a new cluster from the backups,
   following [backup-restore](backup-restore.md). That is a region-loss-sized decision; escalate first.
4. **orders keeps answering `read_only`** after a failover: some connections still point at the demoted primary.
   Restart orders (`kubectl argo rollouts restart orders -n tasuki-prod`) and the two workers
   (`kubectl -n tasuki-prod rollout restart deployment/orders-outbox-relay deployment/orders-dispatch-consumer`).
5. **No standby (`problem=no-standby`).** After a failover the old primary rejoins as a standby by itself (the
   operator rewinds or re-clones it). If its pod stays Pending or crash-looping, fix that (step 2). Do not delete
   instances or PVCs by hand without the owner.
6. **Standby behind (`problem=lag`).** Look for a write burst (bulk deletes from the hourly `purge-expired` CronJob),
   a standby starved of CPU or I/O on its node, or a long query on the standby holding replay. It catches up once the
   cause goes.

## Escalate

Page not mitigated within 30 minutes, or any sign of lost orders: escalate to the owner in
[CODEOWNERS](../../.github/CODEOWNERS).

## Afterwards

After an unplanned failover, compare what clients were told with what the database kept, as GD-06 does: the growth of
`simulator_orders_created_total` against `select count(*) from orders_order where created_at between <from> and <to>;`
(run it with `kubectl cnpg psql orders-db -n tasuki-prod -- -d orders`). Fewer rows means lost confirmed orders: a
postmortem is required. Planned switchovers (minor upgrades with `primaryUpdateStrategy: unsupervised`, `kubectl cnpg
promote`) also fire `OrdersDatabaseFailover`; close those once the operator log confirms them.
