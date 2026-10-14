# Console: the dispatcher's live map and the customer tracking page (services/console).
# Included by the root Makefile (`include mk/*.mk`), which sets SHELL := bash and -eu -o pipefail.

CONSOLE_DIR := services/console

.PHONY: console-install console-lint console-test console-build console-dev console-image

console-install: ## Console: install npm dependencies from the lockfile
	cd $(CONSOLE_DIR) && npm ci

console-lint: ## Console: prettier, eslint and tsc (same as CI)
	cd $(CONSOLE_DIR) && npm run format:check && npm run lint && npm run typecheck

console-test: ## Console: unit tests with the 80% coverage gate on src/lib
	cd $(CONSOLE_DIR) && npm run test:coverage

console-build: ## Console: production build into services/console/dist
	cd $(CONSOLE_DIR) && npm run build

console-dev: ## Console: dev server on :5173 with the mock SSE server on :8090 (Ctrl-C stops both)
	cd $(CONSOLE_DIR) && { npm run mock & mock_pid=$$!; trap 'kill $$mock_pid 2>/dev/null || true' EXIT; npm run dev; }

console-image: ## Console: build the container image tasuki/console:local
	docker build --tag tasuki/console:local $(CONSOLE_DIR)
