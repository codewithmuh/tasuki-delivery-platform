# Game days (docs/gameday/): the fault-injection and observation scripts in scripts/gameday/.
# Included by the root Makefile, which sets SHELL to bash with -eu -o pipefail. Neither target needs a cluster.
# ShellCheck 0.9+; without a local install: make gameday-lint SHELLCHECK="uvx --from shellcheck-py shellcheck"

SHELLCHECK ?= shellcheck
GAMEDAY_DIR := scripts/gameday
GAMEDAY_SCRIPTS := $(wildcard $(GAMEDAY_DIR)/*.sh $(GAMEDAY_DIR)/lib/*.sh $(GAMEDAY_DIR)/local/*.sh)

.PHONY: gameday-lint gameday-test

gameday-lint: ## Game days: bash -n and shellcheck on scripts/gameday, including the local (Compose) drills
	@# The root Makefile requires GNU Make 4 (gmake on macOS); set -e again anyway, as the other mk files do.
	set -eu -o pipefail; for f in $(GAMEDAY_SCRIPTS); do bash -n "$$f"; done
	$(SHELLCHECK) --external-sources $(GAMEDAY_SCRIPTS)

gameday-test: ## Game days: prove every script's guards (help, missing args, prod, unknown context); no cluster
	$(GAMEDAY_DIR)/test-guards.sh
