output "cluster_name" {
  description = "EKS cluster name (Karpenter settings.clusterName)."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint."
  value       = module.eks.cluster_endpoint
}

output "kubeconfig_command" {
  description = "Adds this cluster to ~/.kube/config."
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name} --alias ${module.eks.cluster_name}"
}

output "region" {
  description = "AWS region."
  value       = var.region
}

output "account_id" {
  description = "AWS account the platform runs in (read by deploy/ to register the cluster in Argo CD)."
  value       = var.account_id
}

output "vpc_id" {
  description = "VPC ID. The AWS Load Balancer Controller needs it as a Helm value, because pods cannot read it from instance metadata."
  value       = module.vpc.vpc_id
}

output "karpenter_queue_name" {
  description = "SQS queue for spot interruption events (Karpenter settings.interruptionQueue)."
  value       = module.karpenter.queue_name
}

output "karpenter_node_role_name" {
  description = "IAM role for Karpenter-launched nodes (EC2NodeClass spec.role)."
  value       = module.karpenter.node_iam_role_name
}

output "backup_bucket" {
  description = "S3 bucket for PostgreSQL backups (from infra/bootstrap). bootstrap.sh passes it to Argo CD (cluster annotation backup_bucket)."
  value       = data.aws_s3_bucket.backups.id
}

output "backup_session" {
  description = "When this platform was created; this session's backups go under <namespace>/<session>/ (cluster annotation backup_session)."
  value       = terraform_data.backup_session.output
}
