package kit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return New(Config{Service: "test", Version: "test", Addr: "127.0.0.1:0", ShutdownTimeout: 2 * time.Second}, discard())
}

func get(t *testing.T, h http.Handler, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

func TestHealthz(t *testing.T) {
	rec := get(t, newTestServer(t).Handler(), "/healthz", nil)
	if rec.Code != http.StatusOK || decode(t, rec)["status"] != "ok" {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestReadyzFollowsServingAndChecks(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	if rec := get(t, h, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before serving: got %d, want 503", rec.Code)
	}
	s.SetServing(true)
	if rec := get(t, h, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("serving, no checks: got %d, want 200", rec.Code)
	}
	var failing sync.Mutex
	fail := true
	s.AddReadinessCheck("replay", func(context.Context) error {
		failing.Lock()
		defer failing.Unlock()
		if fail {
			return errors.New("secret internal detail 10.0.0.1")
		}
		return nil
	})
	rec := get(t, h, "/readyz", nil)
	body := decode(t, rec)
	if rec.Code != http.StatusServiceUnavailable || body["reason"] != "replay" {
		t.Fatalf("failing check: got %d %v", rec.Code, body)
	}
	failing.Lock()
	fail = false
	failing.Unlock()
	if rec := get(t, h, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("passing check: got %d", rec.Code)
	}
}

func TestRequestIDEchoedOrGenerated(t *testing.T) {
	s := newTestServer(t)
	var seen string
	s.Handle("GET /id", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = RequestID(r.Context()) }))
	h := s.Handler()
	if got := get(t, h, "/id", map[string]string{"X-Request-Id": "abc-1.2_3"}).Header().Get("X-Request-Id"); got != "abc-1.2_3" || seen != got {
		t.Fatalf("echoed id = %q, context id = %q", got, seen)
	}
	if got := get(t, h, "/id", nil).Header().Get("X-Request-Id"); len(got) != 32 {
		t.Fatalf("generated id = %q", got)
	}
	// Anything that could smuggle content into logs is replaced, not echoed.
	if got := get(t, h, "/id", map[string]string{"X-Request-Id": "bad id\nwith newline"}).Header().Get("X-Request-Id"); len(got) != 32 {
		t.Fatalf("unsafe id was echoed: %q", got)
	}
	if RequestID(context.Background()) != "" {
		t.Fatal("RequestID without a request must be empty")
	}
}

func TestMetricsCountRequests(t *testing.T) {
	h := newTestServer(t).Handler()
	get(t, h, "/healthz", nil)
	get(t, h, "/does-not-exist", nil)
	body := get(t, h, "/metrics", nil).Body.String()
	for _, want := range []string{
		`http_requests_total{method="GET",route="GET /healthz",status="200"} 1`,
		`route="unmatched"`,
		`http_request_duration_seconds_bucket`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestWriteErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusTeapot, "short_and_stout", "I'm a teapot")
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTeapot || body.Error.Code != "short_and_stout" || body.Error.Message != "I'm a teapot" ||
		rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %+v", rec.Code, body)
	}
}

func TestStatusRecorderUnwrapsForFlush(t *testing.T) {
	s := newTestServer(t)
	s.Handle("GET /stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through the wrapper failed: %v", err)
		}
	}))
	if rec := get(t, s.Handler(), "/stream", nil); !rec.Flushed {
		t.Fatal("response was not flushed")
	}
}

// freePort returns a port that was free a moment ago.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunShutsDownInOrder(t *testing.T) {
	port := freePort(t)
	s := New(Config{Addr: "127.0.0.1:" + port, ShutdownTimeout: 2 * time.Second}, discard())
	var mu sync.Mutex
	var order []string
	record := func(step string) {
		mu.Lock()
		order = append(order, step)
		mu.Unlock()
	}
	s.OnStop("first registered", func(context.Context) error { record("closer 1"); return nil })
	s.OnStop("second registered", func(context.Context) error { record("closer 2"); return nil })
	stopped := make(chan struct{})
	go func() { <-s.Stopping(); close(stopped) }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx, Worker{Name: "w", Run: func(ctx context.Context) error {
			<-ctx.Done()
			record("worker")
			return nil
		}})
	}()
	url := "http://127.0.0.1:" + port
	waitFor(t, "readiness", func() bool { return Probe(context.Background(), url+"/readyz") == nil })
	if Healthcheck(context.Background(), func(string) string { return port }) != 0 {
		t.Fatal("healthcheck subcommand failed against a running server")
	}
	// The client may hold a spare connection that never sent a request; the server would wait
	// up to 5 s before treating it as idle. Real probes are one request per process.
	http.DefaultClient.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	<-stopped
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "worker,closer 2,closer 1" {
		t.Fatalf("shutdown order = %v", order)
	}
}

func TestRunStopsWhenAWorkerFails(t *testing.T) {
	s := newTestServer(t)
	closed := false
	s.OnStop("res", func(context.Context) error { closed = true; return errors.New("close failed") })
	err := s.Run(context.Background(), Worker{Name: "broken", Run: func(context.Context) error {
		return errors.New("boom")
	}})
	if err == nil || !strings.Contains(err.Error(), "worker broken: boom") || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("Run returned %v", err)
	}
	if !closed {
		t.Fatal("closers must run after a worker failure")
	}
}

func TestRunReportsWorkersThatIgnoreShutdown(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: 100 * time.Millisecond}, discard())
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := s.Run(ctx, Worker{Name: "stuck", Run: func(context.Context) error { <-release; return nil }})
	if err == nil || !strings.Contains(err.Error(), "did not stop in time") {
		t.Fatalf("Run returned %v", err)
	}
}

func TestRunFailsOnBadAddress(t *testing.T) {
	s := New(Config{Addr: "bad-address"}, discard())
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("expected an error for an invalid address")
	}
}

func TestProbe(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t).Handler())
	defer ts.Close()
	if err := Probe(context.Background(), ts.URL+"/healthz"); err != nil {
		t.Fatalf("healthy endpoint: %v", err)
	}
	if err := Probe(context.Background(), ts.URL+"/readyz"); err == nil {
		t.Fatal("expected an error for a 503 endpoint")
	}
	if err := Probe(context.Background(), "http://127.0.0.1:1/healthz"); err == nil {
		t.Fatal("expected an error when nothing is listening")
	}
	if err := Probe(context.Background(), "://bad"); err == nil {
		t.Fatal("expected an error for an invalid URL")
	}
	closedPort := freePort(t)
	if Healthcheck(context.Background(), func(string) string { return closedPort }) != 1 {
		t.Fatal("healthcheck must fail when nothing listens")
	}
}
