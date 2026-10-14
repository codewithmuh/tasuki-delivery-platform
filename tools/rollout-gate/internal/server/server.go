// Package server serves the gate over HTTP with the platform's runtime contract: probes,
// Prometheus metrics, request IDs, JSON access logs and graceful shutdown.
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
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/codewithmuh/tasuki-delivery-platform/tools/rollout-gate/internal/gate"
)

// Server owns the handler, its readiness flag and its metrics registry.
type Server struct {
	checker   *gate.Checker
	logger    *slog.Logger
	ready     atomic.Bool
	registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	decisions *prometheus.CounterVec
}

// New creates a Server with its own metrics registry (keeps tests independent).
func New(checker *gate.Checker, logger *slog.Logger) *Server {
	s := &Server{
		checker:  checker,
		logger:   logger,
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "HTTP requests.",
		}, []string{"method", "route", "status"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP request latency.", Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		// Labels stay bounded: service and SLO names come from callers, so they go to logs, not labels.
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rollout_gate_decisions_total", Help: "Gate decisions by outcome and cause.",
		}, []string{"allowed", "cause"}),
	}
	s.registry.MustRegister(s.requests, s.latency, s.decisions,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return s
}

// SetReady marks the server as able (or not) to receive traffic.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Handler returns the routed, instrumented HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness does not check Prometheus on purpose: when Prometheus is down the gate must still
	// answer (with a fail-closed decision), not vanish from the Service.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /v1/gate", s.gate)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return s.observe(mux)
}

// gate always answers 200 with a decision, so Argo Rollouts evaluates `allowed` instead of
// counting an error. Only invalid input gets a 400.
func (s *Server) gate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d, err := s.checker.Check(r.Context(), q.Get("service"), q.Get("slo"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalid_argument", "message": err.Error()}})
		return
	}
	s.decisions.WithLabelValues(strconv.FormatBool(d.Allowed), d.Cause).Inc()
	s.logger.Info("decision", "service", d.Service, "slo", d.SLO, "allowed", d.Allowed,
		"cause", d.Cause, "remaining", d.Remaining, "reason", d.Reason)
	writeJSON(w, http.StatusOK, d)
}

// observe adds a request ID, records metrics and writes one JSON access-log line per request.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			b := make([]byte, 16)
			_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-Id", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		route := r.Pattern // unmatched paths share one label, so random URLs can't explode cardinality
		if route == "" {
			route = "unmatched"
		}
		s.requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		s.latency.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())
		s.logger.Info("request", "request_id", id, "method", r.Method, "route", route,
			"status", rec.status, "duration_ms", float64(elapsed.Microseconds())/1000)
	})
}

// Run serves on addr until ctx is cancelled, then fails readiness and drains within shutdownTimeout.
func Run(ctx context.Context, addr string, shutdownTimeout time.Duration, s *Server) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second, // above the default 5 s Prometheus query timeout
		IdleTimeout:       60 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	s.SetReady(true)
	s.logger.Info("listening", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	s.SetReady(false)
	s.logger.Info("shutting down", "timeout", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
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
