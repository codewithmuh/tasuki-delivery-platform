# Grafana dashboards as code: Python (standard library only) → JSON, checked with promtool.
# Included by the root Makefile, which sets SHELL to bash with -eu -o pipefail.
# Tools: python3 (3.10+) and promtool (3.x, version pinned in .github/workflows/dashboards.yml).

PYTHON ?= python3
PROMTOOL ?= promtool
DASHBOARDS_DIR := observability/dashboards

.PHONY: dashboards-generate dashboards-lint

dashboards-generate: ## Render the Grafana dashboards, their kustomization and README metric table (needs python3)
	$(PYTHON) -B $(DASHBOARDS_DIR)/src/build.py

dashboards-lint: ## Test the generator, check generated files are fresh, lint dashboards and PromQL (needs python3, promtool)
	$(PYTHON) -B -m unittest discover --start-directory $(DASHBOARDS_DIR)/src --pattern 'test_*.py'
	$(PYTHON) -B $(DASHBOARDS_DIR)/src/build.py --check
	$(PYTHON) -B $(DASHBOARDS_DIR)/src/lint.py --promtool "$(PROMTOOL)"
