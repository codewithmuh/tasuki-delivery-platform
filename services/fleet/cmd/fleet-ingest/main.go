// Command fleet-ingest accepts courier GPS batches and publishes them to Kafka (courier.locations).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/ingest"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	// `fleet-ingest healthcheck` lets Docker probe the service without a shell in the image.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(kit.Healthcheck(context.Background(), os.Getenv))
	}

	logger := kit.NewLogger(os.Stdout, kit.ParseLevel(os.Getenv("LOG_LEVEL")), ingest.Service, version)

	// SIGTERM is what Kubernetes sends before stopping a container.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := ingest.Run(ctx, os.Getenv, logger, version); err != nil {
		logger.Error("stopped with an error", "err", err)
		stop()
		os.Exit(1)
	}
}
