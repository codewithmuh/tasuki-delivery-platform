# TFLint for every stack under infra/. Run from this folder (make infra-check does):
#   tflint --init && tflint --recursive --config "$PWD/.tflint.hcl"

config {
  # Lint our own code. Registry modules are pinned, versioned and tested upstream.
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

# AWS rules: invalid instance types, regions, IAM policy syntax and the like.
plugin "aws" {
  enabled = true
  version = "0.49.0"
  source  = "github.com/terraform-linters/tflint-ruleset-aws"
}
