package kit

import (
	"io"
	"log/slog"
	"strings"
	"time"
)

// Config holds the settings every Tasuki Go service shares (docs/architecture.md, runtime contract).
type Config struct {
	Service         string        // binary name: fleet-ingest, fleet-state, tracking
	Version         string        // set at build time with -ldflags "-X main.version=..."
	Addr            string        // listen address, from PORT (default 8080)
	ShutdownTimeout time.Duration // SHUTDOWN_TIMEOUT: how long a SIGTERM may take in total (default 20s)
}

// LoadConfig reads the shared settings from the environment.
func LoadConfig(e *Env, service, version string) Config {
	return Config{
		Service:         service,
		Version:         version,
		Addr:            ":" + e.String("PORT", "8080"),
		ShutdownTimeout: e.Duration("SHUTDOWN_TIMEOUT", 20*time.Second),
	}
}

// ParseLevel maps LOG_LEVEL values (debug, info, warn, error) to slog levels; anything else is info.
func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(s))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// NewLogger returns a JSON logger that stamps every line with the service name and version,
// so logs from all three binaries can be told apart in Loki without relying on pod labels.
func NewLogger(w io.Writer, level slog.Level, service, version string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})).
		With("service", service, "version", version)
}
