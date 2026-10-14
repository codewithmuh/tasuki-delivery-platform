# Runbook: orders-db backups failing, and restoring from them

Alert: `OrdersDatabaseBackupFailing` (`severity: ticket`, both environments), label `problem` ·
[rules](../../observability/alerts/postgres.yaml) · setup and full YAML:
[deploy/README.md](../../deploy/README.md#backups-and-restore) · game day
[GD-08](../gameday/experiments/08-orders-db-backup-restore.md)

| `problem` | Fires when |
|---|---|
| `last-backup-failed` | The newest base backup attempt failed (newer than the newest success), for 5 minutes |
| `no-recent-backup` | No successful base backup for 3 h in prod (hourly schedule) or 26 h in dev (daily), for 30 minutes. Also 30 minutes into a session whose first backup never completed |
| `wal-archiving` | WAL archive attempts fail with none succeeding for 15 minutes, or more than five WAL segments wait to be archived, for 10 minutes |

The backup metrics (`barman_cloud_cloudnative_pg_io_*`) come from the Barman Cloud plugin and have not been seen on a
cluster yet. If the alert never fires and these queries return nothing, the names are the first suspect:
`count by (__name__) ({__name__=~"barman_cloud.+", namespace="tasuki-prod"})`.

## Symptoms

How backups work (names from `deploy/` and `infra/`):

| Piece | What |
|---|---|
| Bucket `tasuki-pg-backups-<account-id>` | `infra/bootstrap`, outlives `make aws-down`; path `tasuki-<env>/<session>/orders-db/{base,wals}/` |
| ObjectStore `orders-db-backups` | One per namespace, retention dev 7 days, prod 30 days |
| WAL archiving | `orders-db`'s `spec.plugins` (`barman-cloud.cloudnative-pg.io`, `isWALArchiver: true`): each segment as it fills, or every 5 minutes (`archive_timeout`). This is the RPO |
| ScheduledBackup `orders-db` | Base backups: one at once (`immediate: true`), then dev daily 03:00 JST, prod hourly. They bound how much WAL a restore replays (the RTO) |
| Pod Identity | ServiceAccount `orders-db` → role `tasuki-<env>-orders-db-backup` (read/write); `orders-db-restore` → `tasuki-<env>-orders-db-restore` (read-only) |

The plugin runs as a sidecar, container `plugin-barman-cloud`, in every instance pod.

## Impact

Nothing user-visible now. If the database's volumes are lost while backups are failing, the restore point is the last
archived WAL segment (`wal-archiving`) or needs a longer replay from an older base backup (`last-backup-failed`,
`no-recent-backup`). Failing WAL archiving also keeps WAL in `pg_wal`, which fills the 5 Gi volume over time and then
stops the database (kube-prometheus-stack's `KubePersistentVolumeFillingUp` watches that).

## Confirm

```bash
NS=tasuki-prod
kubectl -n $NS get cluster orders-db -o jsonpath='{.status.conditions[?(@.type=="ContinuousArchiving")]}{"\n"}'
kubectl -n $NS get backups --sort-by=.metadata.creationTimestamp | tail -5         # orders-db-<timestamp>, phase
kubectl -n $NS describe backup <name>                                             # the error of a failed one
kubectl -n $NS get scheduledbackup orders-db
kubectl -n $NS get objectstore orders-db-backups -o jsonpath='{.status.serverRecoveryWindow}{"\n"}'
kubectl cnpg status orders-db -n $NS                                              # includes continuous backup status
kubectl -n $NS logs <primary> -c plugin-barman-cloud --tail=100 | grep -iE 'error|denied|wal'
kubectl -n cnpg-system logs deployment/barman-cloud --tail=100                    # the plugin's manager
```

```promql
max by (namespace) (time() - barman_cloud_cloudnative_pg_io_last_available_backup_timestamp)   # age of the newest backup
cnpg_collector_pg_wal_archive_status{value="ready"}                                             # WAL waiting to be archived
increase(cnpg_pg_stat_archiver_failed_count[15m])
cnpg_pg_stat_archiver_seconds_since_last_archival
```

## Mitigate

1. **`AccessDenied` / `403` in the sidecar log:** the Pod Identity association or role is missing or wrong. Check in
   AWS (`aws eks list-pod-identity-associations --cluster-name tasuki --namespace tasuki-prod`) that
   `orders-db` is associated with `tasuki-prod-orders-db-backup`. Fix it in `infra/platform/backups.tf`, never with a
   key in the cluster.
2. **`NoSuchBucket`:** the bootstrap stack's bucket is missing, or the ObjectStore's path is wrong (it comes from the
   cluster annotations `backup_bucket` and `backup_session`, written by `deploy/bootstrap/bootstrap.sh`).
3. **WAL archiving refused because the archive is not empty:** CloudNativePG will not archive into a path that
   already has WAL from another cluster. Each session has its own `<session>` path for this reason; a cluster
   recreated within the same session needs a new session value.
4. **One base backup failed and the next one is far away** (dev runs daily): take one now, after the cause is fixed:
   `kubectl cnpg backup orders-db -n $NS --method=plugin --plugin-name=barman-cloud.cloudnative-pg.io`.
5. **WAL piling up and the volume filling:** fixing archiving drains it. If the volume is about to fill first, grow
   it (`storage.size` in `deploy/apps/base/postgres.yaml`, through a pull request), never delete WAL by hand.

## Restore (into a new cluster)

A restore never overwrites `orders-db`: it creates a new cluster, **always named `orders-db-restore`**, because only
that ServiceAccount has the read-only restore role and a NetworkPolicy (`deploy/apps/base/network-policies.yaml`).
The full manifest, including a restore from an earlier session's path, is in
[deploy/README.md](../../deploy/README.md#backups-and-restore). Its core:

```yaml
spec:
  bootstrap:
    recovery:
      source: origin
      recoveryTarget:
        targetTime: "2026-10-14 10:30:00+09:00"   # omit to replay everything archived
  externalClusters:
    - name: origin
      plugin:
        name: barman-cloud.cloudnative-pg.io
        parameters: {barmanObjectName: orders-db-backups, serverName: orders-db}
```

1. Pick the target time: just before the damage (a bad migration, a bulk delete), or nothing for "as late as possible".
2. `kubectl apply -f orders-db-restore.yaml`, then watch `kubectl -n $NS get cluster orders-db-restore -w` until it
   is healthy. Note the elapsed time: that is the RTO.
3. **Verify** before anyone relies on it. `scripts/gameday/restore-check.sh` prints row counts and checksums per
   table (read-only; for `tasuki-prod` it wants `ALLOW_PROD=yes`): run it with `--cluster orders-db-restore --out
   copy.tsv` and, while the source is alive, `--cluster orders-db --out source.tsv`, then `--compare source.tsv
   copy.tsv` (always with `--context tasuki --namespace $NS`). `select max(created_at) from orders_order;` on the copy
   gives the newest order it holds: the RPO actually achieved.
4. **Use it:** copy the rows that were lost back into `orders-db`, or, if `orders-db` is gone, decide with the owner
   how to promote the copy (pointing orders at `orders-db-restore-rw` is a change to `deploy/`, and the copy needs its
   own WAL archiving under a new session path). Never give `orders-db-restore` `isWALArchiver: true` on
   `orders-db-backups`: it would write into the source's archive.
5. Delete the copy when done: `kubectl -n $NS delete cluster orders-db-restore`.

## Escalate

Ticket: fix within the day in prod (the RPO is degrading while it lasts). A real restore is an incident: escalate to the
owner in [CODEOWNERS](../../.github/CODEOWNERS) before step 4.

## Afterwards

Record the measured RTO and RPO next to GD-08's targets (RPO ≤ 15 minutes, RTO ≤ 4 hours, design §10). A backup that
has never been restored is a hope, not a backup: run GD-08 Phase B after any change to the backup setup.
