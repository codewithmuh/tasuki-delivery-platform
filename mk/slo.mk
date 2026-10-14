# SLOs as code: Sloth specs → Prometheus rules, promtool checks and tests, and the Go rollout gate.
# Included by the root Makefile, which sets SHELL to bash with -eu -o pipefail.
# Tools: sloth and promtool (versions pinned in .github/workflows/slo.yml), go and golangci-lint.

SLOTH ?= sloth
PROMTOOL ?= promtool
SLO_PERIOD := 28d
SLO_DIR := observability/slo
ALERTS_DIR := observability/alerts
GATE_DIR := tools/rollout-gate

.PHONY: slo-generate slo-check gate-lint gate-test

slo-generate: ## Generate Prometheus SLO rules from the Sloth specs (needs sloth)
	$(SLOTH) validate --no-log --default-slo-period=$(SLO_PERIOD) -i $(SLO_DIR)/specs
	rm -f $(SLO_DIR)/generated/*.yaml
	@# set -e again: macOS ships GNU Make 3.81, which ignores .SHELLFLAGS.
	set -eu -o pipefail; for spec in $(SLO_DIR)/specs/*.yaml; do \
	  out="$(SLO_DIR)/generated/$$(basename "$$spec")"; \
	  { echo "# Generated from $$spec by 'make slo-generate' (sloth --default-slo-period=$(SLO_PERIOD))."; \
	    echo "# Edit the spec and regenerate; CI fails when this file is stale."; \
	    $(SLOTH) generate --no-log --default-slo-period=$(SLO_PERIOD) -i "$$spec"; } > "$$out"; \
	  echo "wrote $$out"; \
	done

slo-check: ## Check and unit-test the SLO and alert rules (needs promtool)
	$(PROMTOOL) check rules $(SLO_DIR)/generated/*.yaml $(ALERTS_DIR)/*.yaml
	$(PROMTOOL) test rules $(SLO_DIR)/tests/*.yaml $(ALERTS_DIR)/tests/*.yaml

gate-lint: ## Lint the rollout gate (needs golangci-lint)
	cd $(GATE_DIR) && gofmt -l . | (! grep .) && go vet ./... && golangci-lint run ./...

gate-test: ## Test the rollout gate (race detector, coverage)
	cd $(GATE_DIR) && go test -race -cover ./...
