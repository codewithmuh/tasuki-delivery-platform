package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// server serves the simulator's own probes and metrics. The simulator is a service like any
// other: Kubernetes probes it and Prometheus scrapes it.
type server struct {
	http  *http.Server
	ln    net.Listener
	ready atomic.Bool
	done  chan error
}

// startServer listens on addr and serves in the background. /healthz answers at once;
// /readyz answers 200 only after setReady(true).
func startServer(ctx context.Context, addr string, gatherer prometheus.Gatherer, logger *slog.Logger) (*server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	s := &server{ln: ln, done: make(chan error, 1)}
	s.http = &http.Server{
		Handler:           s.handler(gatherer),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() { s.done <- s.http.Serve(ln) }()
	logger.Info("listening", "addr", ln.Addr().String())
	return s, nil
}

func (s *server) handler(gatherer prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	// Liveness: the process is up. It never checks dependencies.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: seeded and generating load. False while seeding and while draining.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	return mux
}

func (s *server) setReady(v bool) { s.ready.Store(v) }

// shutdown stops accepting connections and waits up to timeout for open requests.
func (s *server) shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-s.done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the status line is already sent; nothing useful to do on error
}

// probeClient makes one-off health checks. Without keep-alive, a check never leaves a spare
// connection open that the server would have to wait for at shutdown.
var probeClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// Probe checks a health endpoint and returns an error unless it answers 200. It powers the
// `simulator healthcheck` subcommand, because the distroless image has no shell or curl.
func Probe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
	}
	return nil
}
