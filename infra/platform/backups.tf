# Who may use the PostgreSQL backup bucket (made by infra/bootstrap/backups.tf, which outlives this stack).
# Per environment namespace, two roles bound with EKS Pod Identity to CloudNativePG ServiceAccounts (the operator
# names a cluster's ServiceAccount after the cluster):
#   <ns>-orders-db-backup   orders-db          read and write <ns>/*: archives WAL, takes and prunes base backups
#   <ns>-orders-db-restore  orders-db-restore  read <ns>/*: a restore into a new cluster (deploy/README.md)
# The whole namespace prefix, not this session's path, so a new session can restore an older session's backups.

locals {
  # Names fixed by infra/bootstrap/backups.tf.
  backup_bucket    = "tasuki-pg-backups-${var.account_id}"
  backup_key_alias = "alias/tasuki-pg-backups"

  backup_namespaces = toset(["tasuki-dev", "tasuki-prod"])
}

# Looked up rather than assumed, so a plan fails here (not the first backup, in the cluster) if infra/bootstrap
# has not been applied since the bucket was added: `make aws-bootstrap`.
data "aws_s3_bucket" "backups" {
  bucket = local.backup_bucket
}

data "aws_kms_alias" "backups" {
  name = local.backup_key_alias
}

# When this platform was created, e.g. 20261014T0930Z: the path this session's clusters archive under
# (deploy/charts/postgres-backup). CloudNativePG refuses to archive WAL into an archive that already holds WAL
# from another cluster, and every `make aws-up` creates the clusters anew, so each session needs its own path.
# Fixed when created (ignore_changes) and destroyed by `make aws-down`. To start a fresh path within a session
# (orders-db itself was re-created): TF_CLI_ARGS_apply="-replace=terraform_data.backup_session" make aws-up,
# then make gitops-bootstrap.
resource "terraform_data" "backup_session" {
  input = formatdate("YYYYMMDD'T'hhmm'Z'", plantimestamp())

  lifecycle {
    ignore_changes = [input]
  }
}

module "orders_db_backup_pod_identity" {
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "2.9.0"

  for_each = local.backup_namespaces

  # Exact names (no random suffix), so runbooks and CloudTrail searches can name the role.
  name                      = "${each.key}-orders-db-backup"
  use_name_prefix           = false
  attach_custom_policy      = true
  custom_policy_description = "CloudNativePG orders-db in ${each.key}: WAL archive and base backups under ${each.key}/"

  policy_statements = [
    {
      # Barman checks that the bucket exists (HeadBucket), which IAM authorises as s3:ListBucket and which
      # carries no prefix, so listing cannot be narrowed to the prefix. Reading and writing objects can.
      sid       = "FindTheBucket"
      actions   = ["s3:ListBucket", "s3:GetBucketLocation"]
      resources = [data.aws_s3_bucket.backups.arn]
    },
    {
      # Delete: Barman's retention policy removes backups older than the recovery window.
      sid       = "ReadWriteOwnPrefix"
      actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
      resources = ["${data.aws_s3_bucket.backups.arn}/${each.key}/*"]
    },
    {
      # The bucket encrypts with its KMS key; S3 calls KMS on the role's behalf, and only S3 may.
      sid       = "UseTheBucketKey"
      actions   = ["kms:GenerateDataKey", "kms:Decrypt"]
      resources = [data.aws_kms_alias.backups.target_key_arn]
      condition = [{
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["s3.${var.region}.amazonaws.com"]
      }]
    },
  ]

  associations = {
    this = {
      cluster_name    = module.eks.cluster_name
      namespace       = each.key
      service_account = "orders-db"
    }
  }
}

# Read-only: a restore must never write into the archive it restores from.
module "orders_db_restore_pod_identity" {
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "2.9.0"

  for_each = local.backup_namespaces

  name                      = "${each.key}-orders-db-restore"
  use_name_prefix           = false
  attach_custom_policy      = true
  custom_policy_description = "CloudNativePG orders-db-restore in ${each.key}: read backups under ${each.key}/"

  policy_statements = [
    {
      sid       = "FindTheBucket"
      actions   = ["s3:ListBucket", "s3:GetBucketLocation"]
      resources = [data.aws_s3_bucket.backups.arn]
    },
    {
      sid       = "ReadOwnPrefix"
      actions   = ["s3:GetObject"]
      resources = ["${data.aws_s3_bucket.backups.arn}/${each.key}/*"]
    },
    {
      sid       = "DecryptWithTheBucketKey"
      actions   = ["kms:Decrypt"]
      resources = [data.aws_kms_alias.backups.target_key_arn]
      condition = [{
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["s3.${var.region}.amazonaws.com"]
      }]
    },
  ]

  associations = {
    this = {
      cluster_name    = module.eks.cluster_name
      namespace       = each.key
      service_account = "orders-db-restore"
    }
  }
}
