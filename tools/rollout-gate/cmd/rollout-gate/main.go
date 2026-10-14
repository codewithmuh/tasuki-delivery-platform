// Command rollout-gate blocks canary releases of a service whose SLO error budget is spent.
// It runs as an HTTP service for Argo Rollouts (serve) or as a one-shot CLI for CI and humans (check).
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/tools/rollout-gate/internal/gate"
	"github.com/codewithmuh/tasuki-delivery-platform/tools/rollout-gate/internal/server"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

const usage = `usage:
  rollout-gate serve        serve GET /v1/gate?service=<service>&slo=<slo> on $PORT
  rollout-gate check --service <service> --slo <slo> [--prometheus-url <url>]
                            print the decision as JSON; exit 0 allowed, 1 blocked, 2 error

environment (defaults): PROMETHEUS_URL (http://localhost:9090), MIN_BUDGET_REMAINING (0),
  FAIL_MODE (closed | open; closed), PORT (8080), SHUTDOWN_TIMEOUT (20s), LOG_LEVEL (info)`

func main() {
	// SIGTERM is what Kubernetes sends before stopping a container.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process globals, so tests can drive every subcommand.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, 2, usage)
	}
	cfg, err := gate.ConfigFromEnv(getenv)
	if err != nil {
		return fail(stderr, 2, "config: "+err.Error())
	}
	switch args[0] {
	case "serve":
		return serve(ctx, cfg, getenv, stdout)
	case "check":
		return check(ctx, cfg, args[1:], stdout, stderr)
	default:
		return fail(stderr, 2, usage)
	}
}

func serve(ctx context.Context, cfg gate.Config, getenv func(string) string, stdout io.Writer) int {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cmp.Or(getenv("LOG_LEVEL"), "info"))); err != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: level}))
	shutdown := 20 * time.Second
	if d, err := time.ParseDuration(getenv("SHUTDOWN_TIMEOUT")); err == nil && d > 0 {
		shutdown = d
	}
	logger.Info("starting", "version", version, "prometheus_url", cfg.PrometheusURL,
		"fail_mode", cfg.FailMode, "min_budget_remaining", cfg.MinRemaining)
	s := server.New(gate.NewChecker(cfg, &http.Client{}), logger)
	if err := server.Run(ctx, ":"+cmp.Or(getenv("PORT"), "8080"), shutdown, s); err != nil {
		logger.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

func check(ctx context.Context, cfg gate.Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	service := fs.String("service", "", "service name as in the Sloth spec, e.g. orders")
	slo := fs.String("slo", "", "SLO name as in the Sloth spec, e.g. availability")
	fs.StringVar(&cfg.PrometheusURL, "prometheus-url", cfg.PrometheusURL, "Prometheus base URL (env PROMETHEUS_URL)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d, err := gate.NewChecker(cfg, &http.Client{}).Check(ctx, *service, *slo)
	if err != nil {
		return fail(stderr, 2, err.Error())
	}
	if err := json.NewEncoder(stdout).Encode(d); err != nil {
		return 2
	}
	if !d.Allowed {
		return 1
	}
	return 0
}

func fail(w io.Writer, code int, msg string) int {
	_, _ = fmt.Fprintln(w, msg)
	return code
}
