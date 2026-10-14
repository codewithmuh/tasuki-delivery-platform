// Command server runs the sample Go HTTP service.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/codewithmuh/production-repo-template/services/api-go/internal/server"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	// `server healthcheck` lets Docker/Compose probe the service without a shell in the image.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		if err := server.Probe(context.Background(), "http://127.0.0.1:"+port+"/healthz"); err != nil {
			os.Exit(1)
		}
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: server.ParseLevel(os.Getenv("LOG_LEVEL"))}))

	// SIGTERM is what Kubernetes and ECS send before stopping a container.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := server.ConfigFromEnv(os.Getenv)
	cfg.Version = version
	if err := server.Run(ctx, cfg, logger); err != nil {
		logger.Error("server stopped", "err", err)
		stop()
		os.Exit(1)
	}
}
