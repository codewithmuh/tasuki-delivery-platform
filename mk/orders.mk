# Orders service (services/orders): Django + DRF, PostgreSQL, transactional outbox to Kafka.
# Included by the root Makefile, which sets SHELL to bash with -eu -o pipefail. Details:
# services/orders/README.md.

ORDERS_DIR := services/orders
# The test settings carry fake secrets, so schema and migration checks need no real ones.
ORDERS_TOOLS_ENV := DJANGO_SETTINGS_MODULE=tests.settings PYTHONPATH=src:.

.PHONY: orders-lint orders-test orders-migrate orders-openapi

orders-lint: ## Orders: format check, lint, strict types, migrations in sync (same as CI)
	cd $(ORDERS_DIR) && uv run ruff format --check . && uv run ruff check . && uv run mypy
	cd $(ORDERS_DIR) && $(ORDERS_TOOLS_ENV) uv run python manage.py makemigrations --check --dry-run

orders-test: ## Orders: tests, 80% coverage gate (set DATABASE_URL=postgres://... for the race tests)
	cd $(ORDERS_DIR) && uv run pytest

orders-migrate: ## Orders: apply migrations (needs DATABASE_URL and the secrets, see the README)
	cd $(ORDERS_DIR) && uv run python manage.py migrate --noinput

orders-openapi: ## Orders: regenerate services/orders/openapi.yaml from the code
	cd $(ORDERS_DIR) && $(ORDERS_TOOLS_ENV) uv run python manage.py spectacular \
		--file openapi.yaml --validate --fail-on-warn
