# Simulator (services/simulator) and routing (services/routing) targets.
# Included by the root Makefile, which sets SHELL to bash with -eu -o pipefail.

SIM_DIR := services/simulator
ROUTING_DIR := services/routing
ROUTING_IMAGE ?= tasuki/routing:local

# Where sim-run finds the local stack (override on the command line, e.g. ORDERS_URL=...).
ORDERS_URL ?= http://127.0.0.1:8000
FLEET_INGEST_URL ?= http://127.0.0.1:8081
DISPATCH_URL ?= http://127.0.0.1:8082
ROUTING_URL ?= http://127.0.0.1:5000
SIM_PORT ?= 8090

.PHONY: sim-lint sim-test sim-run sim-validate routing-build routing-run

sim-lint: ## Simulator: gofmt, go vet and golangci-lint (same as CI)
	cd $(SIM_DIR) && test -z "$$(gofmt -l cmd internal scenarios)" || { gofmt -l cmd internal scenarios; exit 1; }
	cd $(SIM_DIR) && go vet ./... && golangci-lint run ./...

sim-test: ## Simulator: tests with the race detector and coverage
	cd $(SIM_DIR) && go test -race -cover ./...

sim-validate: ## Simulator: check every built-in scenario file
	cd $(SIM_DIR) && go run ./cmd/simulator validate

sim-run: ## Simulator: run scenarios/small.yaml against the local stack (needs ADMIN_TOKEN)
	cd $(SIM_DIR) && ORDERS_URL=$(ORDERS_URL) FLEET_INGEST_URL=$(FLEET_INGEST_URL) \
		DISPATCH_URL=$(DISPATCH_URL) ROUTING_URL=$(ROUTING_URL) PORT=$(SIM_PORT) \
		go run ./cmd/simulator run -scenario scenarios/small.yaml

routing-build: ## Routing: build the OSRM Tokyo image (downloads ~500 MB; slow)
	docker build --tag $(ROUTING_IMAGE) $(ROUTING_DIR)

routing-run: ## Routing: serve the OSRM image on http://127.0.0.1:5000
	docker run --rm --read-only --cap-drop ALL --publish 127.0.0.1:5000:5000 $(ROUTING_IMAGE)
