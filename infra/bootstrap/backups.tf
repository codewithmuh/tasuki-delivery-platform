# PostgreSQL backups: base backups and archived WAL from CloudNativePG's Barman Cloud plugin
# (deploy/apps/base/postgres.yaml). The bucket lives in this stack, which is applied once per account, because
# infra/platform is destroyed after every session (make aws-down), and backups that die with the cluster are not
# backups. Who may write to it: infra/platform/backups.tf (one Pod Identity role per environment).
#
# Layout: s3://tasuki-pg-backups-<account>/<namespace>/<session>/<cluster>/{base,wals}/
#   <namespace>  tasuki-dev or tasuki-prod; each namespace's role may only use its own prefix
#   <session>    when that platform was created (infra/platform: terraform_data.backup_session)

locals {
  backup_bucket = "tasuki-pg-backups-${var.account_id}"

  # Days an object stays after it is written, per environment prefix. The Barman retention policy (the
  # ObjectStores, deploy/platform/tasuki.yaml: dev 7d, prod 30d) deletes older backups while a cluster runs; these
  # rules delete what sessions that no longer run left behind. Retention plus a week, so the base backup a
  # recovery window still needs is never removed under a running cluster.
  backup_expiration_days = {
    "tasuki-dev"  = 14
    "tasuki-prod" = 37
  }
}

# A customer managed key rather than SSE-S3: the key policy and CloudTrail decide and record who can read the
# backups (orders hold customer addresses), independently of the bucket policy. About $1 a month.
resource "aws_kms_key" "backups" {
  description             = "Tasuki PostgreSQL backups (default encryption of ${local.backup_bucket})"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.backups_key.json
}

resource "aws_kms_alias" "backups" {
  # infra/platform finds the key by this name.
  name          = "alias/tasuki-pg-backups"
  target_key_id = aws_kms_key.backups.key_id
}

# The default key policy, written out: the account's IAM policies decide who may use the key (the backup roles
# in infra/platform get encrypt and decrypt through theirs). In a key policy, Resource "*" means this key.
data "aws_iam_policy_document" "backups_key" {
  #checkov:skip=CKV_AWS_109:Key policy: Resource "*" is this key only, and the root principal delegates to IAM policies.
  #checkov:skip=CKV_AWS_111:Key policy: Resource "*" is this key only, and the root principal delegates to IAM policies.
  #checkov:skip=CKV_AWS_356:Key policy: Resource "*" is this key only, and the root principal delegates to IAM policies.
  statement {
    sid       = "AccountIamPolicies"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["arn:aws:iam::${var.account_id}:root"]
    }
  }
}

resource "aws_s3_bucket" "backups" {
  #checkov:skip=CKV_AWS_18:Access logs need a second bucket; only the backup roles touch this one, and their KMS use is in CloudTrail. Proposed — ADR pending.
  #checkov:skip=CKV_AWS_144:The copy to Osaka (ap-northeast-3) belongs to the region-loss design (game day GD-12), not built yet.
  #checkov:skip=CKV2_AWS_62:Nothing consumes bucket event notifications.
  bucket = local.backup_bucket

  # Terraform never deletes a bucket that still holds backups; emptying it is a deliberate, manual step.
  force_destroy = false
}

resource "aws_s3_bucket_ownership_controls" "backups" {
  bucket = aws_s3_bucket.backups.id

  # No ACLs: access is decided by IAM and the bucket policy only.
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "backups" {
  bucket = aws_s3_bucket.backups.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# A deleted or overwritten backup stays recoverable as a noncurrent version for a week (lifecycle below), which
# covers a bad retention run or a compromised writer.
resource "aws_s3_bucket_versioning" "backups" {
  bucket = aws_s3_bucket.backups.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.backups.arn
    }
    # One data key per bucket for a while instead of one KMS call per object: WAL means many small objects.
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id

  dynamic "rule" {
    for_each = local.backup_expiration_days

    content {
      id     = "expire-${rule.key}"
      status = "Enabled"

      filter {
        prefix = "${rule.key}/"
      }

      expiration {
        days = rule.value
      }

      noncurrent_version_expiration {
        noncurrent_days = 7
      }
    }
  }

  # Whole bucket: once every version of an object has expired, its delete marker is left on its own; remove it
  # so listings (which Barman does on every backup) stay short. A base backup interrupted mid-upload (pod killed,
  # node lost) leaves parts that are billed but invisible; abort them after a day.
  rule {
    id     = "clean-up"
    status = "Enabled"

    filter {}

    expiration {
      expired_object_delete_marker = true
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }

  # Versioning must be on before rules about noncurrent versions apply.
  depends_on = [aws_s3_bucket_versioning.backups]
}

data "aws_iam_policy_document" "backups_bucket" {
  # Requests without TLS are refused, whoever makes them.
  statement {
    sid     = "TlsOnly"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.backups.arn,
      "${aws_s3_bucket.backups.arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "backups" {
  bucket = aws_s3_bucket.backups.id
  policy = data.aws_iam_policy_document.backups_bucket.json

  # The public access block must exist first, or a policy could briefly apply without it.
  depends_on = [aws_s3_bucket_public_access_block.backups]
}
