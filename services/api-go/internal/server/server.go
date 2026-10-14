// Package server implements the HTTP service: probes, Prometheus metrics, request IDs,
// structured access logs and graceful shutdown.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Config holds runtime settings, read from the environment (12-factor).
type Config struct {
	Addr            string
	Version         string
	ShutdownTimeout time.Duration
}

// ConfigFromEnv builds a Config from environment lookups; getenv is os.Getenv in production.
func ConfigFromEnv(getenv func(string) string) Config {
	cfg := Config{Addr: ":8080", Version: "dev", ShutdownTimeout: 20 * time.Second}
	if p := getenv("PORT"); p != "" {
		cfg.Addr = ":" + p
	}
	if d, err := time.ParseDuration(getenv("SHUTDOWN_TIMEOUT")); err == nil && d > 0 {
		cfg.ShutdownTimeout = d
	}
	return cfg
}

// ParseLevel maps LOG_LEVEL values (debug, info, warn, error) to slog levels; default info.
func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(s))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Server owns the handler, its readiness flag and its metrics registry.
type Server struct {
	cfg      Config
	logger   *slog.Logger
	ready    atomic.Bool
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
}

// New creates a Server with its own metrics registry (keeps tests independent).
func New(cfg Config, logger *slog.Logger) *Server {
	s := &Server{
		cfg:      cfg,
		logger:   logger,
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "HTTP requests.",
		}, []string{"method", "route", "status"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP request latency.", Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	s.registry.MustRegister(s.requests, s.latency,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return s
}

// SetReady marks the server as able (or not) to receive traffic.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Handler returns the routed, instrumented HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Liveness: the process is up. It never checks dependencies (that causes restart storms).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: safe to receive traffic. Add dependency checks (DB, cache) here.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /v1/hello", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "world"
		}
		if len(name) > 64 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "name must be at most 64 characters"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": fmt.Sprintf("Hello, %s!", name)})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return s.observe(mux)
}

// observe adds a request ID, records metrics and writes one JSON access-log line per request.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		// ServeMux sets r.Pattern to the matched route; unmatched paths share one label so
		// random 404 URLs can't explode metric cardinality.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		s.latency.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())
		s.logger.Info("request", "request_id", id, "method", r.Method, "route", route,
			"status", rec.status, "duration_ms", float64(elapsed.Microseconds())/1000)
	})
}

// Run serves until ctx is cancelled, then shuts down gracefully within cfg.ShutdownTimeout.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	s := New(cfg, logger)
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	s.SetReady(true)
	logger.Info("listening", "addr", ln.Addr().String(), "version", cfg.Version)

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// Fail readiness first so the load balancer stops sending traffic, then drain.
	s.SetReady(false)
	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// Probe checks a health endpoint and returns an error unless it answers 200. It powers the
// `server healthcheck` subcommand, because the distroless image has no shell or curl.
func Probe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
	}
	return nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the status line is already sent; nothing useful to do on error
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms
	return hex.EncodeToString(b)
}
