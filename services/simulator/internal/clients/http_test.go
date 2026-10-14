package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is an Observer that remembers what it saw.
type recorder struct {
	mu      sync.Mutex
	calls   []string // "call outcome"
	retries []string
}

func (r *recorder) ObserveRequest(call, outcome string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call+" "+outcome)
}

func (r *recorder) Retried(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries = append(r.retries, call)
}

func (r *recorder) snapshot() (calls, retries []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...), append([]string(nil), r.retries...)
}

// fastRetry keeps retry tests quick.
var fastRetry = Retry{Attempts: 4, Base: time.Millisecond, Max: 5 * time.Millisecond}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error": {"code": %q, "message": "test"}}`, code)
}

func TestReason(t *testing.T) {
	cases := map[string]error{
		"ok":           nil,
		"http_503":     &APIError{Status: 503},
		"timeout":      fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
		"cancelled":    context.Canceled,
		"bad_response": fmt.Errorf("%w: eof", errBadResponse),
		"network":      errors.New("connection refused"),
	}
	for want, err := range cases {
		if got := Reason(err); got != want {
			t.Errorf("Reason(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestRetryable(t *testing.T) {
	retry := []error{&APIError{Status: 500}, &APIError{Status: 503}, &APIError{Status: 429}, context.DeadlineExceeded, errors.New("connection reset")}
	for _, err := range retry {
		if !Retryable(err) {
			t.Errorf("%v should be retryable", err)
		}
	}
	noRetry := []error{&APIError{Status: 400}, &APIError{Status: 409}, &APIError{Status: 422}, context.Canceled, errBadResponse}
	for _, err := range noRetry {
		if Retryable(err) {
			t.Errorf("%v should not be retryable", err)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	p := Retry{Attempts: 5, Base: 100 * time.Millisecond, Max: time.Second}
	// The server's Retry-After wins, capped at a minute.
	if got := p.Delay(1, &APIError{Status: 503, RetryAfter: 7 * time.Second}); got != 7*time.Second {
		t.Errorf("Retry-After 7s: delay %v", got)
	}
	if got := p.Delay(1, &APIError{Status: 503, RetryAfter: time.Hour}); got != time.Minute {
		t.Errorf("Retry-After 1h: delay %v, want the 1m cap", got)
	}
	// Otherwise full jitter: anywhere in (0, Base × 2^(n-1)], never above Max.
	for n, ceiling := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 4: 800 * time.Millisecond, 5: time.Second, 70: time.Second} {
		for range 200 {
			if got := p.Delay(n, errors.New("x")); got <= 0 || got > ceiling {
				t.Fatalf("retry %d: delay %v outside (0, %v]", n, got, ceiling)
			}
		}
	}
	if got := (Retry{}).Delay(1, nil); got != 0 {
		t.Errorf("zero policy: delay %v, want 0", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got < 25*time.Second || got > 31*time.Second {
		t.Errorf("HTTP date: %v, want about 30s", got)
	}
	for in, want := range map[string]time.Duration{"": 0, "5": 5 * time.Second, "soon": 0, "-3": 0} {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNonTasukiErrorBodyIsKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream connect error", http.StatusBadGateway)
	}))
	defer srv.Close()
	e := newEndpoint(srv.URL, nil, time.Second, nil)
	_, err := e.do(context.Background(), request{call: "x", method: http.MethodGet, path: "/"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || !strings.Contains(apiErr.Message, "upstream connect error") {
		t.Fatalf("got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "HTTP 502: ") {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestBadJSONIsABadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()
	e := newEndpoint(srv.URL, nil, time.Second, nil)
	var out map[string]any
	_, err := e.do(context.Background(), request{call: "x", method: http.MethodGet, path: "/"}, &out)
	if Reason(err) != "bad_response" || Retryable(err) {
		t.Fatalf("got %v (reason %s)", err, Reason(err))
	}
}

func TestUnreachableServiceIsANetworkError(t *testing.T) {
	e := newEndpoint("http://127.0.0.1:1", nil, time.Second, nil)
	_, err := e.do(context.Background(), request{call: "x", method: http.MethodGet, path: "/"}, nil)
	if Reason(err) != "network" || !Retryable(err) {
		t.Fatalf("got %v (reason %s)", err, Reason(err))
	}
	if _, err := e.do(context.Background(), request{call: "x", method: "BAD METHOD", path: "/"}, nil); err == nil {
		t.Fatal("expected an error for an invalid method")
	}
	if _, err := e.do(context.Background(), request{call: "x", method: http.MethodPost, path: "/", body: make(chan int)}, nil); err == nil {
		t.Fatal("expected an error for a body that cannot be JSON")
	}
}
