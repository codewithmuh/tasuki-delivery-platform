package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := New(Config{Version: "test"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return s, s.Handler()
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
	_, h := newTestServer(t)
	rec := get(t, h, "/healthz", nil)
	if rec.Code != http.StatusOK || decode(t, rec)["status"] != "ok" {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	s, h := newTestServer(t)
	if rec := get(t, h, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before ready: got %d, want 503", rec.Code)
	}
	s.SetReady(true)
	if rec := get(t, h, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("after ready: got %d, want 200", rec.Code)
	}
}

func TestHello(t *testing.T) {
	_, h := newTestServer(t)
	tests := []struct {
		name, query string
		wantCode    int
		wantMessage string
	}{
		{"default name", "", http.StatusOK, "Hello, world!"},
		{"custom name", "?name=Tokyo", http.StatusOK, "Hello, Tokyo!"},
		{"name too long", "?name=" + strings.Repeat("x", 65), http.StatusUnprocessableEntity, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, h, "/v1/hello"+tc.query, nil)
			if rec.Code != tc.wantCode {
				t.Fatalf("got %d, want %d", rec.Code, tc.wantCode)
			}
			if tc.wantMessage != "" && decode(t, rec)["message"] != tc.wantMessage {
				t.Fatalf("unexpected message")
			}
		})
	}
}

func TestRequestIDEchoedOrGenerated(t *testing.T) {
	_, h := newTestServer(t)
	if got := get(t, h, "/healthz", map[string]string{"X-Request-Id": "abc"}).Header().Get("X-Request-Id"); got != "abc" {
		t.Fatalf("echoed id = %q", got)
	}
	if got := get(t, h, "/healthz", nil).Header().Get("X-Request-Id"); len(got) != 32 {
		t.Fatalf("generated id = %q", got)
	}
}

func TestMetricsCountRequests(t *testing.T) {
	_, h := newTestServer(t)
	get(t, h, "/healthz", nil)
	get(t, h, "/does-not-exist", nil)
	body := get(t, h, "/metrics", nil).Body.String()
	for _, want := range []string{
		`http_requests_total{method="GET",route="GET /healthz",status="200"} 1`,
		`route="unmatched"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{"PORT": "9090", "SHUTDOWN_TIMEOUT": "5s"}
	cfg := ConfigFromEnv(func(k string) string { return env[k] })
	if cfg.Addr != ":9090" || cfg.ShutdownTimeout != 5*time.Second {
		t.Fatalf("got %+v", cfg)
	}
	if d := ConfigFromEnv(func(string) string { return "" }); d.Addr != ":8080" || d.ShutdownTimeout != 20*time.Second {
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

func TestRunShutsDownGracefully(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := Config{Addr: "127.0.0.1:0", Version: "test", ShutdownTimeout: time.Second}
	go func() { done <- Run(ctx, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunFailsOnBadAddress(t *testing.T) {
	err := Run(context.Background(), Config{Addr: "bad-address"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected an error for an invalid address")
	}
}

func TestProbe(t *testing.T) {
	_, h := newTestServer(t)
	ts := httptest.NewServer(h)
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
}
