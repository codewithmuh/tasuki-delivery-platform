# Copy to terraform.tfvars (git-ignored) and fill in. Terraform reads it automatically.
# account_id and api_allowed_cidrs are set by `make aws-up` from TASUKI_ACCOUNT_ID and your current IP.

# Required: who gets cluster-admin besides the CI apply role. Use the ARN exactly as IAM shows it,
# including any path (IAM Identity Center roles live under /aws-reserved/sso.amazonaws.com/...).
admin_principal_arns = ["arn:aws:iam::123456789012:role/your-admin-role"]

# Optional overrides
# single_nat_gateway         = false        # one NAT gateway per AZ (costs about 2x more)
# system_node_instance_types = ["t4g.large"]
