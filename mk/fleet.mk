# Location path (services/fleet): fleet-ingest, fleet-state and tracking, one Go module.
# Included by the root Makefile, which sets SHELL to bash with -eu -o pipefail. Details:
# services/fleet/README.md.

FLEET_DIR := services/fleet
FLEET_SERVICES := fleet-ingest fleet-state tracking

.PHONY: fleet-lint fleet-test fleet-build fleet-images

fleet-lint: ## Fleet: gofmt, go vet and golangci-lint (same as CI)
	cd $(FLEET_DIR) && test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }
	cd $(FLEET_DIR) && go vet ./... && golangci-lint run ./...

fleet-test: ## Fleet: tests with the race detector and coverage (no Docker needed)
	cd $(FLEET_DIR) && go test -race -cover ./...

fleet-build: ## Fleet: build every binary into services/fleet/bin/
	cd $(FLEET_DIR) && CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...

fleet-images: ## Fleet: build one container image per binary (fleet-ingest:local, ...)
	for svc in $(FLEET_SERVICES); do \
		docker build --build-arg SERVICE=$$svc --build-arg APP_VERSION=local -t $$svc:local $(FLEET_DIR); \
	done
