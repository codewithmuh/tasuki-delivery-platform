// Command dispatch matches orders to couriers (greedy, P1), streams offers to couriers and
// answers ETA questions for quotes.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/dispatch"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	// `dispatch healthcheck` lets Docker probe the service without a shell in the image. It reads
	// HTTP_PORT (or PORT) like the server does.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(kit.Healthcheck(context.Background(), dispatch.WithHTTPPort(os.Getenv)))
	}

	logger := kit.NewLogger(os.Stdout, kit.ParseLevel(os.Getenv("LOG_LEVEL")), dispatch.Service, version)

	// SIGTERM is what Kubernetes sends before stopping a container.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := dispatch.Run(ctx, os.Getenv, logger, version); err != nil {
		logger.Error("stopped with an error", "err", err)
		stop()
		os.Exit(1)
	}
}
