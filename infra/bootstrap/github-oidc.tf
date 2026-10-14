# GitHub Actions gets short-lived AWS credentials through OIDC, so no access keys are stored in GitHub.
# Two roles, so a pull request can never change anything:
#
#   tasuki-github-plan   pull requests and main   read-only; runs `terraform plan`
#   tasuki-github-apply  aws-tokyo environment    creates and destroys the platform; a reviewer approves each run

locals {
  oidc_provider = "token.actions.githubusercontent.com"

  # Made by infra/scripts/create-state-bucket.sh.
  state_bucket = "tasuki-tfstate-${var.account_id}"

  # Service-linked roles AWS needs the first time the account uses EKS, managed node groups,
  # load balancers, Auto Scaling and spot instances.
  service_linked_role_services = [
    "eks.amazonaws.com",
    "eks-nodegroup.amazonaws.com",
    "elasticloadbalancing.amazonaws.com",
    "autoscaling.amazonaws.com",
    "spot.amazonaws.com",
  ]
}

resource "aws_iam_openid_connect_provider" "github" {
  url            = "https://${local.oidc_provider}"
  client_id_list = ["sts.amazonaws.com"]
  # No thumbprint_list: AWS checks GitHub's certificate against its own trusted CAs.
}

################################################################################
# Plan role: read-only, for pull requests
################################################################################

data "aws_iam_policy_document" "github_plan_trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_provider}:aud"
      values   = ["sts.amazonaws.com"]
    }

    # Pull requests from this repository's branches, and pushes to main. Forks get no OIDC token.
    condition {
      test     = "StringLike"
      variable = "${local.oidc_provider}:sub"
      values = [
        "repo:${var.github_repository}:pull_request",
        "repo:${var.github_repository}:ref:refs/heads/main",
      ]
    }
  }
}

resource "aws_iam_role" "github_plan" {
  name               = "tasuki-github-plan"
  description        = "GitHub Actions: terraform plan on pull requests (read-only)"
  assume_role_policy = data.aws_iam_policy_document.github_plan_trust.json
}

resource "aws_iam_role_policy_attachment" "github_plan_read_only" {
  role       = aws_iam_role.github_plan.name
  policy_arn = "arn:aws:iam::aws:policy/ReadOnlyAccess"
}

# The one write a plan makes: the state lock, which use_lockfile keeps as an object next to the state.
data "aws_iam_policy_document" "github_plan_state_lock" {
  statement {
    sid       = "StateLockFile"
    actions   = ["s3:PutObject", "s3:DeleteObject"]
    resources = ["arn:aws:s3:::${local.state_bucket}/*.tflock"]
  }
}

resource "aws_iam_role_policy" "github_plan_state_lock" {
  name   = "tasuki-state-lock"
  role   = aws_iam_role.github_plan.id
  policy = data.aws_iam_policy_document.github_plan_state_lock.json
}

################################################################################
# Apply role: creates and destroys the platform, only after a reviewer approves
################################################################################

data "aws_iam_policy_document" "github_apply_trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_provider}:aud"
      values   = ["sts.amazonaws.com"]
    }

    # Only jobs that run in the aws-tokyo environment. GitHub holds those jobs until a reviewer approves.
    condition {
      test     = "StringEquals"
      variable = "${local.oidc_provider}:sub"
      values   = ["repo:${var.github_repository}:environment:${var.github_environment}"]
    }
  }
}

resource "aws_iam_role" "github_apply" {
  name               = "tasuki-github-apply"
  description        = "GitHub Actions: create and destroy the Tasuki platform (aws-tokyo environment only)"
  assume_role_policy = data.aws_iam_policy_document.github_apply_trust.json

  # Creating or destroying EKS takes 15-20 minutes each. Two hours keeps the credentials from
  # expiring in the middle of an apply, which would leave the state locked and half-applied.
  max_session_duration = 7200
}

# Every service except IAM, Organizations and account settings. No AdministratorAccess.
resource "aws_iam_role_policy_attachment" "github_apply_power_user" {
  role       = aws_iam_role.github_apply.name
  policy_arn = "arn:aws:iam::aws:policy/PowerUserAccess"
}

# The platform needs IAM roles (EKS, nodes, controllers), so add IAM back, but only for names that
# start with tasuki-. infra/platform names every role and policy that way.
data "aws_iam_policy_document" "github_apply_iam" {
  statement {
    sid     = "ManagePlatformIam"
    actions = ["iam:*"]
    resources = [
      "arn:aws:iam::${var.account_id}:role/tasuki-*",
      "arn:aws:iam::${var.account_id}:policy/tasuki-*",
      "arn:aws:iam::${var.account_id}:instance-profile/tasuki-*",
    ]
  }

  # Read access to IAM in this account and to AWS-managed policies (the ones Terraform attaches).
  statement {
    sid     = "ReadIam"
    actions = ["iam:Get*", "iam:List*"]
    resources = [
      "arn:aws:iam::${var.account_id}:*",
      "arn:aws:iam::aws:policy/*",
    ]
  }

  statement {
    sid       = "CreatePlatformServiceLinkedRoles"
    actions   = ["iam:CreateServiceLinkedRole"]
    resources = ["arn:aws:iam::${var.account_id}:role/aws-service-role/*"]

    condition {
      test     = "StringEquals"
      variable = "iam:AWSServiceName"
      values   = local.service_linked_role_services
    }
  }

  # PowerUserAccess on its own allows creating any service-linked role. Only an explicit Deny can
  # narrow a permission that another policy grants.
  statement {
    sid       = "NoOtherServiceLinkedRoles"
    effect    = "Deny"
    actions   = ["iam:CreateServiceLinkedRole"]
    resources = ["*"]

    condition {
      test     = "StringNotEquals"
      variable = "iam:AWSServiceName"
      values   = local.service_linked_role_services
    }
  }

  # The CI roles are named tasuki-* too. Without this the apply role could edit its own policies,
  # or the plan role's, and grant itself more. Reading them stays allowed.
  statement {
    sid         = "CannotChangeCiRoles"
    effect      = "Deny"
    not_actions = ["iam:Get*", "iam:List*"]
    resources   = ["arn:aws:iam::${var.account_id}:role/tasuki-github-*"]
  }
}

resource "aws_iam_role_policy" "github_apply_iam" {
  name   = "tasuki-platform-iam"
  role   = aws_iam_role.github_apply.id
  policy = data.aws_iam_policy_document.github_apply_iam.json
}
