// Command simulator is Tasuki's city simulator: merchants, customers and couriers in Tokyo,
// driving the platform only through its public APIs. It is the load generator for SLOs and
// game days, and the live demo.
//
// Usage:
//
//	simulator run      [-scenario small] [-seed N] [-duration 1h] [-fresh]
//	simulator seed     [-scenario small] [-seed N] [-fresh]
//	simulator validate [scenario ...]
//	simulator healthcheck
//	simulator version
//
// A scenario is a built-in name (small, normal-weekday, rainy-friday) or a YAML file path.
// Where Tasuki runs comes from environment variables; see the README.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/sim"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/scenarios"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	// SIGTERM is what Kubernetes sends before stopping a container: the simulator then drains.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process-wide parts (arguments, environment, exit), so tests can
// call it. It returns the exit code: 0 ok, 1 failed, 2 bad usage.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: sim.ParseLevel(getenv("LOG_LEVEL"))}))
	cfg := sim.ConfigFromEnv(getenv)
	cfg.Version = version

	switch args[0] {
	case "run", "seed":
		return runOrSeed(ctx, args[0], args[1:], getenv, cfg, logger, stderr)
	case "validate":
		return validate(args[1:], stdout, stderr)
	case "healthcheck":
		port := getenv("PORT")
		if port == "" {
			port = "8080"
		}
		if err := sim.Probe(ctx, "http://127.0.0.1:"+port+"/healthz"); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "version":
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	default:
		usage(stderr)
		return 2
	}
}

func runOrSeed(ctx context.Context, command string, args []string, getenv func(string) string,
	cfg sim.Config, logger *slog.Logger, stderr io.Writer,
) int {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	defaultScenario := getenv("SCENARIO")
	if defaultScenario == "" {
		defaultScenario = "small"
	}
	name := flags.String("scenario", defaultScenario, "built-in scenario name or path to a scenario YAML file (env SCENARIO)")
	seed := flags.Uint64("seed", 0, "override the scenario's seed (0 keeps it)")
	duration := flags.Duration("duration", 0, "stop after this long (0 keeps the scenario's setting)")
	fresh := flags.Bool("fresh", false, "seed new merchants and couriers even if the state file matches")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	sc, err := sim.LoadScenario(*name)
	if err != nil {
		logger.Error("load scenario", "err", err)
		return 1
	}
	if *seed != 0 {
		sc.Seed = *seed
	}
	logger = logger.With("scenario", sc.Name, "version", cfg.Version)

	if command == "seed" {
		orders := clients.NewOrders(cfg.OrdersURL, cfg.AdminToken, nil, nil)
		if _, err := sim.Prepare(ctx, cfg, sc, orders, *fresh, logger); err != nil {
			logger.Error("seed", "err", err)
			return 1
		}
		return 0
	}
	if err := sim.Run(ctx, cfg, sc, sim.Options{Fresh: *fresh, Duration: *duration}, logger); err != nil {
		logger.Error("simulation failed", "err", err)
		return 1
	}
	return 0
}

// validate checks scenario files or built-in names; with no arguments, every built-in one.
func validate(names []string, stdout, stderr io.Writer) int {
	if len(names) == 0 {
		names = scenarios.Names()
	}
	code := 0
	for _, name := range names {
		sc, err := sim.LoadScenario(name)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			code = 1
			continue
		}
		curve, _ := sc.Curve()
		perDay := curve.Expected(0, 24*time.Hour)
		_, _ = fmt.Fprintf(stdout, "%s: ok (%d wards, %d couriers, ~%.0f orders/day before weather)\n",
			name, len(sc.Wards), sc.Couriers.Count, perDay)
	}
	return code
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `usage: simulator <command> [flags]

commands:
  run          seed (or reuse the state file) and run a scenario until stopped
  seed         only create the merchants, stores and couriers, and write the state file
  validate     check scenario files (default: every built-in scenario)
  healthcheck  probe http://127.0.0.1:$PORT/healthz (for container health checks)
  version      print the version

run "simulator run -h" for flags.
`)
}
