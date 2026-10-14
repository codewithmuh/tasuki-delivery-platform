SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# GNU Make 3.81, the version macOS ships, ignores .SHELLFLAGS: a command failing inside a pipe would
# pass silently. Stop early (`make help` still works) and point to a current make.
ifeq ($(filter oneshell,$(.FEATURES)),)
ifneq ($(filter-out help,$(or $(MAKECMDGOALS),help)),)
$(error GNU Make $(MAKE_VERSION) ignores .SHELLFLAGS, so failures could pass silently. Use GNU Make 4: on macOS `brew install make`, then run `gmake $(MAKECMDGOALS)`)
endif
endif

# Each area keeps its own targets in mk/<area>.mk; the targets below run them all.
#   orders.mk     services/orders (Python)      fleet.mk   services/fleet (Go)
#   simulator.mk  services/simulator, routing    console.mk services/console (TypeScript)
#   slo.mk        observability/, rollout gate   infra.mk   infra/ (Terraform, AWS)
#   deploy.mk     deploy/ (GitOps)                dashboards.mk observability/dashboards (Grafana)
-include $(wildcard mk/*.mk)

GO_MODULES := services/fleet services/simulator tools/rollout-gate

# Compose reads .env. Without one (the stack was never configured), fall back to the placeholders so
# that down, logs and build still work; `up` insists on a real .env.
COMPOSE := docker compose$(if $(wildcard .env),, --env-file .env.example)

.PHONY: help setup fmt lint test check scan build up sim-up routing-up ps down reset logs clean env-check

help: ## Show available commands
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

setup: console-install ## Install every area's dependencies and the git hooks (uv, go, node, pre-commit)
	cd services/orders && uv sync
	for mod in $(GO_MODULES); do (cd "$$mod" && go mod download); done
	pre-commit install

fmt: ## Format all code (Python, Go, TypeScript, Terraform)
	cd services/orders && uv run ruff format . && uv run ruff check --fix .
	cd services/fleet && gofmt -w cmd internal
	cd services/simulator && gofmt -w cmd internal scenarios
	cd tools/rollout-gate && gofmt -w .
	cd services/console && npm run format
	$(TERRAFORM) fmt -recursive infra

lint: orders-lint fleet-lint sim-lint console-lint gate-lint slo-check dashboards-lint infra-check deploy-lint ## Lint, type-check and statically check everything (same as CI; `make -k` to see every failure)

test: orders-test fleet-test sim-test sim-validate console-test gate-test ## Run every test suite with its coverage gate (same as CI)

check: lint test ## Everything CI runs before building images

scan: ## Secret and vulnerability scans of the whole repository (needs gitleaks and trivy)
	gitleaks git --redact --no-banner .
	trivy fs --scanners vuln,misconfig,secret --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 \
		--ignorefile .trivyignore.yaml --skip-dirs '**/.terraform' --skip-dirs '**/node_modules' --skip-dirs '**/.venv' .

build: ## Build every container image (the local stack, the simulator, the rollout gate; routing: make routing-build)
	$(COMPOSE) --profile sim build
	docker build --tag tasuki/rollout-gate:local tools/rollout-gate

env-check:
	@test -f .env || { echo "No .env yet. Create it first:  cp .env.example .env" >&2; exit 1; }

up: env-check ## Run the local stack (needs .env), then open the console on http://localhost:8088
	$(COMPOSE) up --build --detach --wait --wait-timeout 600
	$(COMPOSE) ps
	@echo "Console: http://localhost:8088   Orders API: http://localhost:8000   Start the city: make sim-up"

sim-up: env-check ## Start the city simulator against the local stack (SIM_SCENARIO=small; see .env)
	$(COMPOSE) --profile sim up --build --detach --wait --wait-timeout 600 simulator
	@echo "Couriers appear on http://localhost:8088 once seeding finishes. Follow it: docker compose logs -f simulator"

routing-up: env-check ## Build and start OSRM on Tokyo roads (first build ~10-20 min, 4-6 GB RAM)
	$(COMPOSE) --profile routing up --build --detach --wait --wait-timeout 900 routing

ps: ## List the local containers and their health
	$(COMPOSE) --profile '*' ps

down: ## Stop and remove the local containers (data volumes are kept)
	$(COMPOSE) --profile '*' down --remove-orphans

reset: ## Stop the local stack and delete its data (PostgreSQL, Kafka)
	$(COMPOSE) --profile '*' down --remove-orphans --volumes

logs: ## Follow the logs of every local container
	$(COMPOSE) --profile '*' logs --follow --tail 100

clean: ## Remove build, test and cache artifacts (keeps lockfiles and Terraform lock files)
	rm -rf .ruff_cache services/orders/.venv services/orders/.pytest_cache services/orders/.mypy_cache \
		services/orders/.ruff_cache services/orders/.coverage services/orders/coverage.xml services/orders/htmlcov
	rm -rf services/fleet/bin $(addsuffix /coverage.out,$(GO_MODULES))
	rm -rf services/console/node_modules services/console/dist services/console/coverage
	rm -rf infra/bootstrap/.terraform infra/platform/.terraform
