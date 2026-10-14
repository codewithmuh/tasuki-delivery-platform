package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/tools/rollout-gate/internal/gate"
)

// lockedBuffer is a bytes.Buffer safe for the server goroutine (writer) and the test (reader).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func envOf(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

func prometheusWith(t *testing.T, value string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1760000000,"`+value+`"]}]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCheckCommand(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		args     []string
		wantCode int
	}{
		{"allowed exits 0", "0.3", []string{"--service", "orders", "--slo", "availability"}, 0},
		{"blocked exits 1", "-0.01", []string{"--service", "orders", "--slo", "availability"}, 1},
		{"invalid name exits 2", "0.3", []string{"--service", "Orders!", "--slo", "availability"}, 2},
		{"unknown flag exits 2", "0.3", []string{"--nope"}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"check", "--prometheus-url", prometheusWith(t, tc.value)}, tc.args...)
			code := run(context.Background(), args, envOf(nil), &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d (stderr: %s)", code, tc.wantCode, stderr.String())
			}
			if tc.wantCode == 2 {
				return
			}
			var d gate.Decision
			if err := json.Unmarshal(stdout.Bytes(), &d); err != nil || d.Allowed != (tc.wantCode == 0) {
				t.Fatalf("stdout %q: %+v, %v", stdout.String(), d, err)
			}
		})
	}
}

func TestCheckUsesPrometheusURLFromEnv(t *testing.T) {
	var stdout bytes.Buffer
	env := envOf(map[string]string{"PROMETHEUS_URL": prometheusWith(t, "0.9")})
	if code := run(context.Background(), []string{"check", "--service", "tracking", "--slo", "freshness"}, env, &stdout, io.Discard); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stdout.String())
	}
}

func TestUsageAndConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"no subcommand", nil, nil},
		{"unknown subcommand", []string{"deploy"}, nil},
		{"bad fail mode", []string{"check"}, map[string]string{"FAIL_MODE": "maybe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(context.Background(), tc.args, envOf(tc.env), io.Discard, &stderr); code != 2 || stderr.Len() == 0 {
				t.Fatalf("exit %d, stderr %q; want 2 and a message", code, stderr.String())
			}
		})
	}
}

func TestServeStartsAndStopsOnSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var logs lockedBuffer
	done := make(chan int, 1)
	env := envOf(map[string]string{"PORT": "0", "LOG_LEVEL": "debug", "SHUTDOWN_TIMEOUT": "2s"})
	go func() { done <- run(ctx, []string{"serve"}, env, &logs, io.Discard) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `"msg":"listening"`) {
		if time.Now().After(deadline) {
			t.Fatalf("server did not start; logs: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel() // what SIGTERM does in main
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, want 0; logs: %s", code, logs.String())
	}
}

func TestServeFailsWhenPortIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	env := envOf(map[string]string{"PORT": port, "LOG_LEVEL": "nonsense"})
	if code := run(context.Background(), []string{"serve"}, env, io.Discard, io.Discard); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}
