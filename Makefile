SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

PY := services/api-python
GO := services/api-go

.PHONY: help setup fmt lint test check scan build up down logs clean

help: ## Show available commands
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'

setup: ## Install dependencies and the git hooks (needs uv, go, pre-commit)
	cd $(PY) && uv sync
	cd $(GO) && go mod download
	pre-commit install

fmt: ## Format all code
	cd $(PY) && uv run ruff format . && uv run ruff check --fix .
	cd $(GO) && gofmt -w cmd internal

lint: ## Lint and type-check everything (same as CI)
	cd $(PY) && uv run ruff format --check . && uv run ruff check . && uv run mypy
	cd $(GO) && golangci-lint run ./...

test: ## Run all tests with the coverage gates (same as CI)
	cd $(PY) && uv run pytest
	cd $(GO) && go test -race -cover ./...

check: lint test ## Everything CI runs before building images

scan: ## Secret and vulnerability scans (needs gitleaks and trivy)
	gitleaks git --redact --no-banner .
	trivy fs --scanners vuln,misconfig,secret --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 .

build: ## Build all container images
	docker compose build

up: ## Run all services locally (python :8000, go :8080)
	docker compose up --build --detach --wait
	docker compose ps

down: ## Stop and remove local containers
	docker compose down --remove-orphans

logs: ## Follow logs of all services
	docker compose logs --follow

clean: ## Remove build, test and cache artifacts
	rm -rf $(PY)/.venv $(PY)/.pytest_cache $(PY)/.mypy_cache $(PY)/.ruff_cache $(PY)/.coverage $(GO)/coverage.out
