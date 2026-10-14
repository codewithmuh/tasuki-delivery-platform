# Each value goes into a GitHub repository variable (Settings → Secrets and variables → Actions).

output "plan_role_arn" {
  description = "GitHub variable AWS_PLAN_ROLE_ARN: the role terraform.yml assumes to plan pull requests."
  value       = aws_iam_role.github_plan.arn
}

output "apply_role_arn" {
  description = "GitHub variable AWS_APPLY_ROLE_ARN: the role infra-apply.yml assumes to create and destroy the platform."
  value       = aws_iam_role.github_apply.arn
}

output "state_bucket" {
  description = "GitHub variable TF_STATE_BUCKET: the S3 bucket that holds the Terraform state."
  value       = local.state_bucket
}

output "backup_bucket" {
  description = "S3 bucket for PostgreSQL backups (infra/platform reads it by name; it survives make aws-down)."
  value       = aws_s3_bucket.backups.id
}

output "backup_kms_key_arn" {
  description = "KMS key that encrypts the backup bucket (alias/tasuki-pg-backups)."
  value       = aws_kms_key.backups.arn
}
