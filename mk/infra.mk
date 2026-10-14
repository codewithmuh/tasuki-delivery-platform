# Infrastructure: create and destroy the AWS platform, and the static checks CI runs on infra/.
# Included by the root Makefile. How it fits together, and what it costs: infra/README.md.
#
# The aws-* targets and kubeconfig use a real AWS account, so they stop unless both are set and the
# logged-in account matches:
#   export AWS_PROFILE=tasuki              # your personal profile, never the machine's default identity
#   export TASUKI_ACCOUNT_ID=123456789012

TERRAFORM ?= terraform
TFLINT ?= tflint
TRIVY ?= trivy
CHECKOV ?= checkov

INFRA_PLATFORM := infra/platform
INFRA_BOOTSTRAP := infra/bootstrap
INFRA_SCRIPTS := infra/scripts
INFRA_REGION := ap-northeast-1
INFRA_CLUSTER := tasuki

# Each recipe line runs in a new shell; this loads the account guard and helpers into it.
# set -eu -o pipefail again because macOS ships GNU Make 3.81, which ignores .SHELLFLAGS.
INFRA_SH := set -eu -o pipefail; source $(INFRA_SCRIPTS)/common.sh

# The state bucket is per account, so its name is passed at init time and no account ID is committed.
infra_init = $(TERRAFORM) -chdir=$(1) init -input=false -backend-config=bucket=tasuki-tfstate-$(TASUKI_ACCOUNT_ID)

.PHONY: aws-guard aws-bootstrap aws-plan aws-up aws-down aws-leftovers kubeconfig infra-check infra-cost

# Terraform's provider refuses every account except this one (allowed_account_ids).
aws-bootstrap aws-plan aws-up aws-down: export TF_VAR_account_id = $(TASUKI_ACCOUNT_ID)

aws-guard:
	@$(INFRA_SH); require_aws_profile; require_tasuki_account
	@echo "AWS account check passed (profile $$AWS_PROFILE)."

aws-bootstrap: aws-guard ## AWS, once per account: state bucket, GitHub OIDC roles, budget alerts
	$(INFRA_SCRIPTS)/create-state-bucket.sh
	$(call infra_init,$(INFRA_BOOTSTRAP))
	$(TERRAFORM) -chdir=$(INFRA_BOOTSTRAP) apply
	@echo "Next: copy the outputs above into the GitHub repository variables (infra/README.md)."

aws-plan: aws-guard ## AWS: show what aws-up would change (changes nothing)
	$(call infra_init,$(INFRA_PLATFORM))
	$(INFRA_SH); cidrs=$$(api_allowed_cidrs); \
	  TF_VAR_api_allowed_cidrs="$$cidrs" $(TERRAFORM) -chdir=$(INFRA_PLATFORM) plan

aws-up: aws-guard ## AWS: create or update the platform (about $0.30/hour while up); asks before applying
	$(call infra_init,$(INFRA_PLATFORM))
	@echo 'Approximate cost while the platform runs: about $$0.30/hour (~$$7/day), before Karpenter nodes,'
	@echo 'load balancers and data transfer. Exact figures: make infra-cost. Stop paying: make aws-down.'
	$(INFRA_SH); cidrs=$$(api_allowed_cidrs); \
	  TF_VAR_api_allowed_cidrs="$$cidrs" $(TERRAFORM) -chdir=$(INFRA_PLATFORM) apply

aws-down: aws-guard ## AWS: delete what Kubernetes created, destroy the platform, check nothing is left
	$(call infra_init,$(INFRA_PLATFORM))
	$(INFRA_SCRIPTS)/pre-destroy.sh
	$(INFRA_SH); cidrs=$$(api_allowed_cidrs); \
	  TF_VAR_api_allowed_cidrs="$$cidrs" $(TERRAFORM) -chdir=$(INFRA_PLATFORM) destroy
	$(INFRA_SCRIPTS)/check-leftovers.sh

aws-leftovers: aws-guard ## AWS: list anything billable left in Tokyo (exit 1 if there is)
	$(INFRA_SCRIPTS)/check-leftovers.sh

kubeconfig: aws-guard ## AWS: add the EKS cluster to ~/.kube/config as context "tasuki"
	aws eks update-kubeconfig --region $(INFRA_REGION) --name $(INFRA_CLUSTER) --alias $(INFRA_CLUSTER)

infra-check: ## Static checks on infra/, as in CI: fmt, validate, tflint, trivy, checkov (no AWS access)
	$(TERRAFORM) fmt -check -recursive infra
	$(TERRAFORM) -chdir=$(INFRA_BOOTSTRAP) init -backend=false -input=false
	$(TERRAFORM) -chdir=$(INFRA_BOOTSTRAP) validate
	$(TERRAFORM) -chdir=$(INFRA_PLATFORM) init -backend=false -input=false
	$(TERRAFORM) -chdir=$(INFRA_PLATFORM) validate
	cd infra && $(TFLINT) --init && $(TFLINT) --recursive --config "$$PWD/.tflint.hcl"
	@# .terraform/ holds downloaded modules (with their example YAML); trivy fetches and checks those modules anyway.
	$(TRIVY) fs infra --skip-dirs '**/.terraform' --scanners vuln,misconfig,secret --severity CRITICAL,HIGH --ignore-unfixed --exit-code 1
	$(CHECKOV) -d infra --config-file infra/.checkov.yaml --quiet --compact

infra-cost: ## Monthly cost estimate of the platform stack (needs infracost and a free API key)
	@if command -v infracost >/dev/null 2>&1; then \
	  infracost breakdown --path $(INFRA_PLATFORM); \
	else \
	  echo "infracost is not installed. Install it (https://www.infracost.io/docs/) and run 'infracost auth login'."; \
	  echo "Until then, infra/README.md has an approximate hourly cost table."; \
	fi
