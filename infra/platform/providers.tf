provider "aws" {
  region = var.region

  # Refuse to touch any account except Tasuki's, even if the shell is logged in to another one.
  allowed_account_ids = [var.account_id]

  # Every resource Terraform creates carries these tags, so cost reports and leftover checks can find it.
  default_tags {
    tags = local.tags
  }
}

locals {
  tags = {
    Project     = var.name
    Environment = var.environment
    Stack       = "platform"
    ManagedBy   = "terraform"
    Repository  = "codewithmuh/tasuki-delivery-platform"
  }
}
