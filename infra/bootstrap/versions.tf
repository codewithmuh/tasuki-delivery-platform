terraform {
  # 1.10+ for S3-native state locking (use_lockfile); no DynamoDB table needed.
  required_version = ">= 1.10, < 2.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.68"
    }
  }

  # Same bucket as the platform stack, its own key. The bucket is made by
  # infra/scripts/create-state-bucket.sh, because a stack cannot keep its state in a bucket it creates.
  #   terraform init -backend-config=bucket=tasuki-tfstate-<account-id>
  backend "s3" {
    key          = "bootstrap/terraform.tfstate"
    region       = "ap-northeast-1"
    encrypt      = true
    use_lockfile = true
  }
}
