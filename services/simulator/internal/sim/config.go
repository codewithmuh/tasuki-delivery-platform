package sim

import (
	"log/slog"
	"strings"
	"time"
)

// Config is the simulator's runtime configuration, read from environment variables
// (12-factor). The scenario file describes the city; this describes where Tasuki is.
type Config struct {
	OrdersURL      string // orders service: seeding, quotes, orders, pickup, deliver
	FleetIngestURL string // GPS batches
	DispatchURL    string // offer stream, accept and decline
	TrackingURL    string // customers' tracking streams
	RoutingURL     string // OSRM; empty means straight lines only
	AdminToken     string // bearer token for /v1/admin/* (seeding and token re-issue)
	Addr           string // where /healthz, /readyz and /metrics listen
	StateFile      string // seeded API keys and tokens, reused on restart
	// ShutdownTimeout bounds the HTTP server's drain; DrainTimeout bounds how long couriers
	// may take to finish the deliveries in hand after SIGTERM.
	ShutdownTimeout time.Duration
	DrainTimeout    time.Duration
	// SeedWait is how long to wait for the orders service to become ready before seeding.
	SeedWait time.Duration
	Version  string
}

// ConfigFromEnv builds a Config from environment lookups; getenv is os.Getenv in production.
// The defaults suit a local stack started with `make up`.
func ConfigFromEnv(getenv func(string) string) Config {
	cfg := Config{
		OrdersURL:       envOr(getenv, "ORDERS_URL", "http://127.0.0.1:8000"),
		FleetIngestURL:  envOr(getenv, "FLEET_INGEST_URL", "http://127.0.0.1:8081"),
		DispatchURL:     envOr(getenv, "DISPATCH_URL", "http://127.0.0.1:8082"),
		TrackingURL:     envOr(getenv, "TRACKING_URL", "http://127.0.0.1:8083"),
		RoutingURL:      envOr(getenv, "ROUTING_URL", "http://127.0.0.1:5000"),
		AdminToken:      getenv("ADMIN_TOKEN"),
		Addr:            ":" + envOr(getenv, "PORT", "8080"),
		StateFile:       envOr(getenv, "STATE_FILE", ".state/simulator-state.json"),
		ShutdownTimeout: durationOr(getenv, "SHUTDOWN_TIMEOUT", 20*time.Second),
		DrainTimeout:    durationOr(getenv, "DRAIN_TIMEOUT", 60*time.Second),
		SeedWait:        durationOr(getenv, "SEED_WAIT", 2*time.Minute),
		Version:         "dev",
	}
	if strings.EqualFold(cfg.RoutingURL, "none") {
		cfg.RoutingURL = "" // ROUTING_URL=none: no OSRM, couriers ride in straight lines
	}
	return cfg
}

func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return strings.TrimRight(v, "/")
	}
	return fallback
}

func durationOr(getenv func(string) string, key string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(getenv(key)); err == nil && d > 0 {
		return d
	}
	return fallback
}

// ParseLevel maps LOG_LEVEL values (debug, info, warn, error) to slog levels; default info.
func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(s))); err != nil {
		return slog.LevelInfo
	}
	return l
}
