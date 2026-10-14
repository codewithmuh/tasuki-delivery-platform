variable "account_id" {
  description = "The AWS account Tasuki runs in. The provider refuses every other account."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "account_id must be a 12-digit AWS account ID."
  }
}

variable "region" {
  description = "AWS region of the state bucket and the platform."
  type        = string
  default     = "ap-northeast-1"
}

variable "github_repository" {
  description = "owner/name of the only GitHub repository whose workflows may assume the CI roles."
  type        = string
  default     = "codewithmuh/tasuki-delivery-platform"
}

variable "github_environment" {
  description = "GitHub environment that may assume the apply role. Give it a required reviewer in the repo settings."
  type        = string
  default     = "aws-tokyo"
}

variable "monthly_budget_usd" {
  description = "Monthly AWS budget in USD. Alerts go out at 50 % and 80 % of actual spend, and 100 % forecast."
  type        = number
  default     = 100
}

variable "budget_alert_email" {
  description = "Where budget alerts are sent. Set it in terraform.tfvars (git-ignored), never in git."
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("^[^@ ]+@[^@ ]+\\.[^@ ]+$", var.budget_alert_email))
    error_message = "budget_alert_email must be an email address."
  }
}
