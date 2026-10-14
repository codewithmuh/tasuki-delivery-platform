terraform {
  # 1.10+ for S3-native state locking (use_lockfile); no DynamoDB table needed.
  required_version = ">= 1.10, < 2.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.68"
    }
  }

  # The bucket is passed at init time (make aws-up), so no account ID is committed:
  #   terraform init -backend-config=bucket=tasuki-tfstate-<account-id>
  backend "s3" {
    key          = "platform/terraform.tfstate"
    region       = "ap-northeast-1"
    encrypt      = true
    use_lockfile = true
  }
}
