// Package kit is the plumbing every Tasuki Go service shares: configuration from the environment,
// JSON logs, an HTTP server with probes, Prometheus metrics and request IDs, a graceful shutdown
// sequence, and the Kafka consumer loop. It follows the runtime contract in docs/architecture.md.
package kit

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
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Worker is a long-running background task, such as a Kafka consumer or a sweeper. Run must
// return when ctx is cancelled. An error returned before that stops the whole process, so
// workers retry temporary failures (a broker restarting) themselves.
type Worker struct {
	Name string
	Run  func(ctx context.Context) error
}

type check struct {
	name string
	fn   func(context.Context) error
}

type closer struct {
	name string
	fn   func(context.Context) error
}

// Server owns the HTTP handler, readiness state, metrics registry and shutdown hooks of one service.
type Server struct {
	cfg      Config
	logger   *slog.Logger
	mux      *http.ServeMux
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec

	serving  atomic.Bool // true between "listening" and the start of shutdown
	checks   []check
	closers  []closer
	stopping chan struct{}
	stopOnce sync.Once
}

// New creates a Server with its own metrics registry, which keeps tests independent of each other.
func New(cfg Config, logger *slog.Logger) *Server {
	s := &Server{
		cfg:      cfg,
		logger:   logger,
		mux:      http.NewServeMux(),
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "HTTP requests by method, route and status.",
		}, []string{"method", "route", "status"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP request latency by method and route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		stopping: make(chan struct{}),
	}
	s.registry.MustRegister(s.requests, s.latency,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// Liveness: the process is up. It never checks dependencies, because restarting a pod does
	// not fix Kafka or Redis and only turns a dependency outage into a restart storm.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return s
}

// Registry is where a service registers its own metrics, next to the HTTP and Go runtime ones.
func (s *Server) Registry() *prometheus.Registry { return s.registry }

// Logger returns the service logger.
func (s *Server) Logger() *slog.Logger { return s.logger }

// Handle registers an application route, for example "POST /v1/locations".
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// AddReadinessCheck adds a condition that must hold before /readyz answers 200, such as
// "the replay has caught up". Checks run on every probe, so they must be cheap.
func (s *Server) AddReadinessCheck(name string, fn func(context.Context) error) {
	s.checks = append(s.checks, check{name: name, fn: fn})
}

// OnStop registers a cleanup step (flush a producer, close a file) for the end of shutdown.
// Steps run after HTTP has drained and workers have stopped, in reverse order of registration.
func (s *Server) OnStop(name string, fn func(context.Context) error) {
	s.closers = append(s.closers, closer{name: name, fn: fn})
}

// Stopping is closed when shutdown begins. Long-lived responses such as server-sent event
// streams select on it and end, because http.Server.Shutdown waits for them otherwise.
func (s *Server) Stopping() <-chan struct{} { return s.stopping }

// SetServing marks the server as started (true) or shutting down (false). Run calls it; tests may too.
func (s *Server) SetServing(v bool) { s.serving.Store(v) }

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.serving.Load() {
		WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	for _, c := range s.checks {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := c.fn(ctx)
		cancel()
		if err != nil {
			// The reason names the check but not the error: error text can carry internal
			// addresses, and probes may be reachable from outside the cluster.
			s.logger.Warn("not ready", "check", c.name, "err", err)
			WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": c.name})
			return
		}
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// Handler returns the routed, instrumented HTTP handler.
func (s *Server) Handler() http.Handler { return s.observe(s.mux) }

// observe adds a request ID, records metrics and writes one JSON access-log line per request.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		// ServeMux sets r.Pattern to the matched route. Unmatched paths share one label, so random
		// 404 URLs cannot explode metric cardinality, and tokens in paths never reach the logs.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		s.latency.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())
		level := slog.LevelInfo
		if route == "GET /healthz" || route == "GET /readyz" || route == "GET /metrics" {
			level = slog.LevelDebug // probes and scrapes arrive every few seconds; keep them out of info logs
		}
		s.logger.Log(r.Context(), level, "request", "request_id", id, "method", r.Method, "route", route,
			"status", rec.status, "duration_ms", float64(elapsed.Microseconds())/1000)
	})
}

// Run serves HTTP and runs the workers until ctx is cancelled (SIGTERM) or a worker fails. Then
// it shuts down in a fixed order: fail readiness and end streams, drain in-flight requests, stop
// the workers, and run the OnStop steps. The whole sequence shares cfg.ShutdownTimeout.
func (s *Server) Run(ctx context.Context, workers ...Worker) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second, // streaming handlers extend it per write
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}

	// Workers get a context of their own: on SIGTERM they must keep running until HTTP has
	// drained, because in-flight requests may still need them.
	workCtx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWork()
	failed := make(chan error, len(workers)+1) // buffered: a late failure never blocks a goroutine
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() {
			if err := w.Run(workCtx); err != nil && workCtx.Err() == nil {
				failed <- fmt.Errorf("worker %s: %w", w.Name, err)
			}
		})
	}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			failed <- fmt.Errorf("serve: %w", err)
		}
	}()
	s.SetServing(true)
	s.logger.Info("listening", "addr", ln.Addr().String())

	var cause error
	select {
	case <-ctx.Done():
	case cause = <-failed:
		s.logger.Error("stopping after a failure", "err", cause)
	}
	return errors.Join(cause, s.shutdown(srv, stopWork, &wg))
}

func (s *Server) shutdown(srv *http.Server, stopWork context.CancelFunc, wg *sync.WaitGroup) error {
	// 1. Fail readiness so load balancers stop routing here, and tell streams to finish.
	s.SetServing(false)
	s.stopOnce.Do(func() { close(s.stopping) })
	s.logger.Info("shutting down", "timeout", s.cfg.ShutdownTimeout.String())
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	var errs []error
	// 2. Drain in-flight requests.
	if err := srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("drain http: %w", err))
	}
	// 3. Stop the workers and wait for them, within the same time budget.
	stopWork()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, errors.New("workers did not stop in time"))
	}
	// 4. Release resources, last registered first (a consumer closes before the store it writes to).
	for i := len(s.closers) - 1; i >= 0; i-- {
		if err := s.closers[i].fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", s.closers[i].name, err))
		}
	}
	if len(errs) == 0 {
		s.logger.Info("stopped cleanly")
	}
	return errors.Join(errs...)
}

// Probe checks a health endpoint and returns an error unless it answers 200.
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

// Healthcheck implements the `<binary> healthcheck` subcommand used by the Docker HEALTHCHECK: the
// distroless image has no shell or curl, so the binary probes its own /healthz. It returns the
// process exit code.
func Healthcheck(ctx context.Context, getenv func(string) string) int {
	port := getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if err := Probe(ctx, "http://127.0.0.1:"+port+"/healthz"); err != nil {
		return 1
	}
	return 0
}

// WriteJSON writes body as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the status line is already sent; nothing useful to do on error
}

// WriteError writes the contract's error shape: {"error": {"code": "snake_case", "message": "..."}}.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

type requestIDKey struct{}

// RequestID returns the request ID that observe stored in the request context, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// validRequestID bounds what a caller may put in our logs through X-Request-Id.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms
	return hex.EncodeToString(b)
}

// statusRecorder remembers the response status for metrics and logs.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer, so streaming handlers can still
// flush and set write deadlines through this wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
