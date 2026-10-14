package kit

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) *Env {
	return NewEnv(func(k string) string { return m[k] })
}

func TestEnvDefaultsWhenUnset(t *testing.T) {
	e := envFrom(nil)
	if got := e.String("X", "def"); got != "def" {
		t.Errorf("String = %q", got)
	}
	if got := e.Duration("X", time.Second); got != time.Second {
		t.Errorf("Duration = %v", got)
	}
	if got := e.Int("X", 7, 1); got != 7 {
		t.Errorf("Int = %d", got)
	}
	if got := e.Bool("X", true); !got {
		t.Errorf("Bool = %v", got)
	}
	if got := e.List("X", []string{"a"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("List = %v", got)
	}
	if err := e.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
}

func TestEnvParsesValues(t *testing.T) {
	e := envFrom(map[string]string{
		"S": " text ", "D": "2m", "I": "42", "B": "true", "L": "a, b,,c ", "R": "set",
	})
	if e.String("S", "") != "text" || e.Duration("D", 0) != 2*time.Minute || e.Int("I", 0, 1) != 42 ||
		!e.Bool("B", false) || !slices.Equal(e.List("L", nil), []string{"a", "b", "c"}) || e.Required("R") != "set" {
		t.Fatal("unexpected parsed values")
	}
	if err := e.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
}

func TestEnvCollectsEveryError(t *testing.T) {
	e := envFrom(map[string]string{"D": "soon", "N": "-1s", "I": "0", "J": "x", "B": "maybe"})
	e.Duration("D", 0)
	e.Duration("N", 0)
	e.Int("I", 5, 1)
	e.Int("J", 5, 1)
	e.Bool("B", false)
	e.Required("MISSING")
	e.Fail(errors.New("custom problem"))
	err := e.Err()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"D must", "N must", "I must", "J must", "B must", "MISSING is required", "custom problem"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	cfg := LoadConfig(envFrom(map[string]string{"PORT": "9090", "SHUTDOWN_TIMEOUT": "5s"}), "svc", "1.0")
	if cfg.Addr != ":9090" || cfg.ShutdownTimeout != 5*time.Second || cfg.Service != "svc" || cfg.Version != "1.0" {
		t.Fatalf("got %+v", cfg)
	}
	if d := LoadConfig(envFrom(nil), "svc", "dev"); d.Addr != ":8080" || d.ShutdownTimeout != 20*time.Second {
		t.Fatalf("defaults: got %+v", d)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{"debug": slog.LevelDebug, "WARN": slog.LevelWarn, "": slog.LevelInfo, "nonsense": slog.LevelInfo}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewLoggerStampsServiceAndVersion(t *testing.T) {
	var b strings.Builder
	NewLogger(&b, slog.LevelInfo, "fleet-ingest", "1.2.3").Info("hello")
	for _, want := range []string{`"service":"fleet-ingest"`, `"version":"1.2.3"`, `"msg":"hello"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("log line %q is missing %s", b.String(), want)
		}
	}
	NewLogger(io.Discard, slog.LevelError, "x", "y").Info("filtered") // must not panic
}
