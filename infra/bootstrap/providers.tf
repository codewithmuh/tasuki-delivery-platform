provider "aws" {
  region = var.region

  # Refuse to touch any account except Tasuki's, even if the shell is logged in to another one.
  allowed_account_ids = [var.account_id]

  default_tags {
    tags = {
      Project    = "tasuki"
      Stack      = "bootstrap"
      ManagedBy  = "terraform"
      Repository = var.github_repository
    }
  }
}
