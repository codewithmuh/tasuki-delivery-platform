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
