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

	"github.com/codewithmuh/tasuki-delivery-platform/tools/rollout-gate/internal/gate"
)

// newTestServer wires a Server to a fake Prometheus that reports remaining budget `value`.
func newTestServer(t *testing.T, value string) (*Server, http.Handler) {
	t.Helper()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1760000000,"`+value+`"]}]}}`)
	}))
	t.Cleanup(prom.Close)
	checker := gate.NewChecker(gate.Config{PrometheusURL: prom.URL, FailMode: gate.FailClosed}, prom.Client())
	s := New(checker, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return s, s.Handler()
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestProbes(t *testing.T) {
	s, h := newTestServer(t, "0.5")
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d", rec.Code)
	}
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before ready: got %d, want 503", rec.Code)
	}
	s.SetReady(true)
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("readyz after ready: got %d, want 200", rec.Code)
	}
}

func TestGate(t *testing.T) {
	tests := []struct {
		name, value string
		wantAllowed bool
	}{
		{"budget left", "0.42", true},
		{"budget spent", "-0.1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.value)
			rec := get(t, h, "/v1/gate?service=orders&slo=availability")
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
			}
			var d gate.Decision
			if err := json.NewDecoder(rec.Body).Decode(&d); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if d.Allowed != tc.wantAllowed || d.Cause != gate.CauseBudget || d.Remaining == nil {
				t.Fatalf("unexpected decision: %+v", d)
			}
		})
	}
}

func TestGateRejectsBadInput(t *testing.T) {
	_, h := newTestServer(t, "0.5")
	rec := get(t, h, `/v1/gate?service=orders&slo=x%22%7D`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || body.Error.Code != "invalid_argument" {
		t.Fatalf("error body: %+v, %v", body, err)
	}
}

func TestMetricsAndRequestID(t *testing.T) {
	_, h := newTestServer(t, "-0.2")
	get(t, h, "/v1/gate?service=orders&slo=availability")
	get(t, h, "/no/such/path")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "abc123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != "abc123" {
		t.Fatalf("request ID not echoed")
	}
	if id := get(t, h, "/healthz").Header().Get("X-Request-Id"); len(id) != 32 {
		t.Fatalf("generated request ID %q, want 32 hex chars", id)
	}

	body := get(t, h, "/metrics").Body.String()
	for _, want := range []string{
		`rollout_gate_decisions_total{allowed="false",cause="budget"} 1`,
		`http_requests_total{method="GET",route="GET /v1/gate",status="200"} 1`,
		`http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`http_request_duration_seconds_bucket{method="GET",route="GET /v1/gate",le="0.005"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	s, _ := newTestServer(t, "0.5")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, "127.0.0.1:0", time.Second, s) }()

	deadline := time.Now().Add(5 * time.Second)
	for !s.ready.Load() {
		if time.Now().After(deadline) {
			t.Fatal("server never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.ready.Load() {
		t.Fatal("readiness must fail during shutdown")
	}
}

func TestRunFailsOnBadAddress(t *testing.T) {
	s, _ := newTestServer(t, "0.5")
	if err := Run(context.Background(), "not-an-address", time.Second, s); err == nil {
		t.Fatal("want a listen error")
	}
}
