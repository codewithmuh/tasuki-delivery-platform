# GD-08: Orders database backup and restore

| | |
|---|---|
| Status | **Planned, not run.** Both phases can run |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "Region lost" (backups, RPO 15 min, RTO 4 h); §8 (S3 holds database backups); §11 (backups stay in Tokyo, copies only to Osaka) |
| SLOs at stake | none directly (a restore drill runs beside production, not instead of it) |
| Environment | `tasuki-dev` |
| Time box | Phase A 60 minutes; Phase B 2 hours |
| Scripts | `pg-restore.sh`, `restore-check.sh`, `record-timeline.sh` |

`orders-db` now archives WAL and base backups to S3 through the Barman Cloud plugin
([deploy/README.md, "Backups and restore"](../../../deploy/README.md#backups-and-restore)): bucket
`tasuki-pg-backups-<account-id>` (in `infra/bootstrap`, so it survives `make aws-down`), path
`tasuki-<env>/<session>/orders-db/`, ObjectStore `orders-db-backups`, `archive_timeout` 5 minutes, ScheduledBackup
daily in dev and hourly in prod. **A restore always goes into a cluster named `orders-db-restore`**: its ServiceAccount
is the one Pod Identity gives the read-only backup role, and `deploy/apps/base/network-policies.yaml` already admits
the operator and Prometheus to it. A backup that has never been restored is a hope, not a backup: this drill restores one.

Because both phases use the name `orders-db-restore`, **they cannot run at the same time**, and neither can run while
a real restore exists. Delete one before starting the other.

## Hypothesis

**Phase A, a logical copy.** `pg-restore.sh` creates the CloudNativePG cluster `orders-db-restore`, which imports the `orders` database from `orders-db` (`bootstrap.initdb.import`, type
`microservice`, which runs `pg_dump`/`pg_restore` through the operator):

1. The copy reaches `Cluster in healthy state` in **minutes** at dev's data size. That time is the RTO of a logical restore.
2. With the simulator stopped (a quiet database), every table has the **same row count and checksum** in both clusters
   (`restore-check.sh`, which hashes every row's text, ordered).
3. With the simulator running, rows created after the dump started are missing from the copy. That gap is the RPO of
   a logical backup taken at that moment, and it is measured from the newest `orders_order.created_at` in the copy.

**Phase B, physical backups and point-in-time recovery:**

4. A base backup plus continuous WAL archiving to S3 restores into a new cluster (`bootstrap.recovery`) at a chosen
   time: **RPO ≤ the WAL archive interval** (CloudNativePG's default `archive_timeout` is 5 minutes), far inside the
   design's 15 minutes. RTO is the restore time, measured.
5. Row counts and checksums match the source as of the recovery target time.

## What exists now, and what is still missing

Done in `deploy/` and `infra/` since planning: the bucket (survives `aws-down`, versioned, KMS), the Pod Identity
roles (`orders-db` read/write, `orders-db-restore` read-only, per environment prefix), the Barman Cloud plugin, the
ObjectStore and WAL archiving, the ScheduledBackup, the restore cluster's NetworkPolicy, `OrdersDatabaseBackupFailing`
and the [backup-restore](../../runbooks/backup-restore.md) runbook.

Still missing:

- **Phase A's import needs one temporary NetworkPolicy:** `orders-db` admits only the orders processes and its own
  instances, so `orders-db-restore` cannot reach `orders-db:5432` to run `pg_dump`. `pg-restore.sh` creates that
  policy (labelled `tasuki.io/gameday=true`) and deletes it on `--rollback`. Phase B reads S3 and needs nothing extra.
- **A copy to Osaka** (S3 replication to ap-northeast-3) for region loss: [GD-12](12-region-loss.md).

## Steady state

- `kubectl --context tasuki -n tasuki-dev get cluster orders-db` healthy; simulator running (`small`) for at least
  30 minutes, so there is data to copy.
- `restore-check.sh --context tasuki --namespace tasuki-dev --cluster orders-db` works, and prints one line per table.

## Blast radius

A second, temporary PostgreSQL cluster `orders-db-restore` in `tasuki-dev`, labelled `tasuki.io/gameday=true`, plus
(Phase A only) one temporary NetworkPolicy letting it read from `orders-db` on 5432 (the operator and Prometheus may reach the
restore cluster). The import reads the whole database once, which is a little load on dev's only instance. The dev
ResourceQuota (4 CPU, 10 PVCs, 60 Gi) must have room for one more 5 Gi volume and its pod.

## Preconditions

- Preflight GO in `tasuki-dev`.
- For hypothesis 2: the simulator stopped (pull request: `replicas: 0` in `deploy/apps/overlays/dev/values/simulator.yaml`)
  and no write for a minute. Avoid the top of the hour, when the `purge-expired` CronJob deletes rows.
- For hypothesis 3: the simulator running.

## Steps: Phase A

```bash
CTX=tasuki NS=tasuki-dev
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context $CTX --namespace $NS \
  --experiment gd-08-backup-restore --print-path)"
scripts/gameday/preflight.sh --context $CTX --namespace $NS
```

1. **Quiet run (hypothesis 2).** With the simulator stopped:
   `scripts/gameday/restore-check.sh --context $CTX --namespace $NS --cluster orders-db --out /tmp/gd08-source.tsv`.
2. `scripts/gameday/pg-restore.sh --context $CTX --namespace $NS` → it applies the temporary NetworkPolicy and the restore cluster,
   waits until the cluster is healthy, and logs the start, the end and the elapsed time (RTO).
3. `restore-check.sh ... --cluster orders-db-restore --out /tmp/gd08-copy.tsv`, then
   `restore-check.sh ... --compare /tmp/gd08-source.tsv /tmp/gd08-copy.tsv`. Expect identical lines (exit 0).
4. `pg-restore.sh ... --rollback` deletes the copy, its volume and the temporary NetworkPolicy.
5. **Live run (hypothesis 3).** Simulator running. Repeat step 2, note the time the import started (the script logs
   it), then compare `select max(created_at) from orders_order` in both clusters. The difference is the data a logical
   backup taken at that moment would lose. `--rollback` again.
6. `mark end`. Keep the TSV files out of git: they hold only hashes and counts, but they are drill scratch.

## Steps: Phase B (restore from S3, point in time)

Phase A's copy must be gone first (`pg-restore.sh --rollback`): both use the name `orders-db-restore`.

7. Check backups work (deploy/README.md): `ContinuousArchiving` condition True on `orders-db`, at least one completed
   `Backup` (`kubectl -n tasuki-dev get backups`), and the ObjectStore's recovery window. If the latest scheduled backup
   is old, take one: a `Backup` object for `orders-db` with `method: plugin`; wait for `phase: completed`.
8. Write rows the copy must contain (run the simulator for 10 minutes), note time T (JST, with offset), then let it run
   5 more minutes.
9. Apply the `orders-db-restore` manifest from [deploy/README.md, "Backups and restore"](../../../deploy/README.md#backups-and-restore)
   with `recoveryTarget.targetTime: T`. `record-timeline.sh mark inject` when you apply it. Watch
   `kubectl -n tasuki-dev get cluster orders-db-restore -w` until healthy; that elapsed time is the RTO. Follow
   [backup-restore](../../runbooks/backup-restore.md) as the on-call would.
10. `restore-check.sh` on both clusters. Rows with `created_at <= T` must match; rows after T must be absent. The
    newest row before T, compared with T, bounds the RPO.
11. `kubectl -n tasuki-dev delete cluster orders-db-restore` (its volume goes with it). Never give it `spec.plugins`
    with `isWALArchiver: true`: it would archive into the source's path.

## Expected observations

| Signal | Expected |
|---|---|
| Phase A RTO (logical import, dev size) | Minutes |
| Phase A, quiet: per-table rows and checksums | Identical |
| Phase A, live: newest order in the copy | Within seconds of the import start |
| Phase B RPO | ≤ 5 minutes (archive timeout), target ≤ 15 minutes |
| Phase B RTO | Measured; target ≤ 4 hours (design §10, region-level) |

## Abort criteria

In addition to the [global ones](../README.md#abort-criteria-all-experiments):

- The source cluster `orders-db` becomes unhealthy, or orders latency in dev rises sharply during the import.
- The restore cluster is not healthy after 30 minutes (check the import Job's logs).

## Rollback

Phase A: `pg-restore.sh --rollback` deletes the cluster and the temporary NetworkPolicy it created. Phase B:
`kubectl -n tasuki-dev delete cluster orders-db-restore`. Argo CD does not track either, so it neither prunes nor
restores them.

## Alerts and runbooks

| Alert | Should it fire? | Runbook |
|---|---|---|
| `OrdersDatabaseBackupFailing` (ticket; dev and prod) | **No**: a restore only reads the backups. If it fires during the drill, archiving or the schedule is broken, and that is a finding | [backup-restore](../../runbooks/backup-restore.md) |
| Anything else | A restore drill should be silent | — |

## Gaps found while planning

- Phase A needs a temporary NetworkPolicy to reach `orders-db:5432` (above).
- `restore-check.sh` hashes row text, which is exact but reads every row. Fine at dev size; for prod-size tables,
  compare per-day partitions instead.
- Restoring a session's backups after `make aws-down` / `aws-up` needs a hand-made ObjectStore with the old path
  (deploy/README.md). Worth a third run once Phase B has passed.
- Found earlier and since addressed: no bucket, no WAL archive, no Pod Identity role, no backup alerts, no restore
  runbook.

## Results

<!-- fill in after the drill -->

| | |
|---|---|
| Date and window (JST) | <!-- fill in after the drill --> |
| Phase(s) run | <!-- fill in after the drill --> |
| Driver / observer / scribe | <!-- fill in after the drill --> |
| Drill log | <!-- link --> |
| Hypothesis held? | <!-- per numbered point --> |

| Signal | Expected | Observed |
|---|---|---|
| Source size (rows in `orders_order`, database size) | | <!-- fill in after the drill --> |
| Phase A RTO, quiet run (s) | minutes | <!-- fill in after the drill --> |
| Phase A compare result | identical | <!-- fill in after the drill --> |
| Phase A RPO, live run (s) | seconds | <!-- fill in after the drill --> |
| Phase B RPO / RTO | ≤ 5 min / measured | <!-- fill in after the drill --> |

Surprises: <!-- fill in after the drill -->

Action items: <!-- fill in after the drill -->

Postmortem: <!-- link, or "not required" with the reason -->
