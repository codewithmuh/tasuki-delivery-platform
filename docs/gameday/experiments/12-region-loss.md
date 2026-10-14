# GD-12: Region loss (Tokyo)

| | |
|---|---|
| Status | **Blocked**: disaster recovery is designed, not built (design §2 non-goals, §10) |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "Region lost": backups and Kafka-derived history copied to Osaka (ap-northeast-3); target RPO 15 minutes, RTO 4 hours |
| Environment | A separate Osaka environment, created for the drill |

## Hypothesis (to confirm once the pieces exist)

With ap-northeast-1 declared lost (no API calls to it for the drill):

1. A new platform comes up in ap-northeast-3 from the same Terraform and `deploy/` (parameterised by region).
2. The orders database is restored from the Osaka copy of the backups ([GD-08](08-orders-db-backup-restore.md)
   Phase B, cross-region) with **RPO ≤ 15 minutes**.
3. Location history (S3) is available from the Osaka copy; Valkey rebuilds from the new Kafka; Kafka itself starts
   empty, since locations are disposable (design §5).
4. Orders are accepted again within **4 hours** (RTO), and all data stays in Japan (design §11).

## What has to exist first

GD-08 Phase B (backups to S3), S3 replication to Osaka, a region variable through `infra/` and `deploy/` (today
`ap-northeast-1` is fixed in several places: the ClusterSecretStores, Terraform defaults), Secrets Manager replicas
in Osaka, and a written failover runbook with a decision owner.

## Results

<!-- fill in after the drill -->
