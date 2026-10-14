package gate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePrometheus answers /api/v1/query with a vector of the given sample values (as Prometheus
// encodes them: strings), after checking that the gate sent the expected PromQL.
func fakePrometheus(t *testing.T, values ...string) *httptest.Server {
	t.Helper()
	return fakeRaw(t, http.StatusOK, vector(values...))
}

func fakeRaw(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %q, want /api/v1/query", r.URL.Path)
		}
		if got, want := r.URL.Query().Get("query"), Query("orders", "availability"); got != want {
			t.Errorf("query = %q, want %q", got, want)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func vector(values ...string) string {
	series := make([]string, 0, len(values))
	for i, v := range values {
		series = append(series, fmt.Sprintf(`{"metric":{"sloth_service":"orders","sloth_slo":"availability","replica":"%d"},"value":[1760000000,%q]}`, i, v))
	}
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}}`
}

func TestCheck(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // a closed server refuses connections: "Prometheus is down"

	tests := []struct {
		name          string
		url           string
		minRemaining  float64
		failMode      FailMode
		wantAllowed   bool
		wantCause     string
		wantRemaining float64 // checked only when the cause is "budget"
	}{
		{"budget left", fakePrometheus(t, "0.23").URL, 0, FailClosed, true, CauseBudget, 0.23},
		{"budget overspent", fakePrometheus(t, "-0.05").URL, 0, FailClosed, false, CauseBudget, -0.05},
		{"budget exactly at threshold blocks", fakePrometheus(t, "0.25").URL, 0.25, FailClosed, false, CauseBudget, 0.25},
		{"lowest series wins", fakePrometheus(t, "0.8", "0.1").URL, 0.2, FailClosed, false, CauseBudget, 0.1},
		{"NaN series are skipped", fakePrometheus(t, "NaN", "0.4").URL, 0, FailClosed, true, CauseBudget, 0.4},
		{"no data fails closed", fakePrometheus(t).URL, 0, FailClosed, false, CauseNoData, 0},
		{"only NaN is no data", fakePrometheus(t, "NaN", "+Inf").URL, 0, FailOpen, true, CauseNoData, 0},
		{"no data fails open when asked", fakePrometheus(t).URL, 0, FailOpen, true, CauseNoData, 0},
		{"prometheus down fails closed", down.URL, 0, FailClosed, false, CausePrometheusError, 0},
		{"prometheus down fails open when asked", down.URL, 0, FailOpen, true, CausePrometheusError, 0},
		{"prometheus 503", fakeRaw(t, http.StatusServiceUnavailable, `{"status":"error"}`).URL, 0, FailClosed, false, CausePrometheusError, 0},
		{"prometheus error status", fakeRaw(t, http.StatusOK, `{"status":"error"}`).URL, 0, FailClosed, false, CausePrometheusError, 0},
		{"malformed response", fakeRaw(t, http.StatusOK, `not json`).URL, 0, FailClosed, false, CausePrometheusError, 0},
		{"unbuildable URL", "http://bad host", 0, FailClosed, false, CausePrometheusError, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewChecker(Config{PrometheusURL: tc.url + "/", MinRemaining: tc.minRemaining, FailMode: tc.failMode}, http.DefaultClient)
			d, err := c.Check(context.Background(), "orders", "availability")
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if d.Allowed != tc.wantAllowed || d.Cause != tc.wantCause {
				t.Fatalf("got allowed=%v cause=%q (%s), want allowed=%v cause=%q", d.Allowed, d.Cause, d.Reason, tc.wantAllowed, tc.wantCause)
			}
			if d.Service != "orders" || d.SLO != "availability" || d.Threshold != tc.minRemaining || d.Reason == "" {
				t.Fatalf("incomplete decision: %+v", d)
			}
			switch {
			case tc.wantCause == CauseBudget && (d.Remaining == nil || *d.Remaining != tc.wantRemaining):
				t.Fatalf("remaining = %v, want %v", d.Remaining, tc.wantRemaining)
			case tc.wantCause != CauseBudget && d.Remaining != nil:
				t.Fatalf("remaining = %v, want null when the budget is unknown", *d.Remaining)
			}
		})
	}
}

func TestCheckWholeService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Query().Get("query"), Query("orders", ""); got != want {
			t.Errorf("query = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"sloth_service":"orders","sloth_slo":"availability"},"value":[1760000000,"0.6"]},
			{"metric":{"sloth_service":"orders","sloth_slo":"latency"},"value":[1760000000,"-0.2"]}]}}`))
	}))
	defer srv.Close()
	d, err := NewChecker(Config{PrometheusURL: srv.URL}, srv.Client()).Check(context.Background(), "orders", "")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Allowed || d.SLO != "latency" || d.Remaining == nil || *d.Remaining != -0.2 {
		t.Fatalf("want latency to block the whole service, got %+v", d)
	}
}

func TestCheckRejectsInvalidNames(t *testing.T) {
	c := NewChecker(Config{PrometheusURL: "http://unused"}, http.DefaultClient)
	for _, tc := range []struct{ service, slo string }{
		{"", "availability"},
		{"Orders", "availability"},
		{"orders", `availability"} or vector(1) #`},
		{"-orders", "availability"},
		{strings.Repeat("a", 64), "availability"},
	} {
		if _, err := c.Check(context.Background(), tc.service, tc.slo); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Check(%q, %q) error = %v, want ErrInvalidName", tc.service, tc.slo, err)
		}
	}
}

func TestQuery(t *testing.T) {
	for _, tc := range []struct{ service, slo, want string }{
		{"fleet-state", "location-freshness", `slo:period_error_budget_remaining:ratio{sloth_service="fleet-state",sloth_slo="location-freshness"}`},
		{"orders", "", `slo:period_error_budget_remaining:ratio{sloth_service="orders"}`},
	} {
		if got := Query(tc.service, tc.slo); got != tc.want {
			t.Errorf("Query(%q, %q) = %s, want %s", tc.service, tc.slo, got, tc.want)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

	cfg, err := ConfigFromEnv(env(nil))
	if err != nil || cfg.PrometheusURL != "http://localhost:9090" || cfg.MinRemaining != 0 || cfg.FailMode != FailClosed {
		t.Fatalf("defaults: %+v, %v", cfg, err)
	}

	cfg, err = ConfigFromEnv(env(map[string]string{
		"PROMETHEUS_URL": "http://prometheus:9090", "MIN_BUDGET_REMAINING": "0.25", "FAIL_MODE": "open",
	}))
	if err != nil || cfg.PrometheusURL != "http://prometheus:9090" || cfg.MinRemaining != 0.25 || cfg.FailMode != FailOpen {
		t.Fatalf("overrides: %+v, %v", cfg, err)
	}

	for _, bad := range []map[string]string{
		{"MIN_BUDGET_REMAINING": "abc"},
		{"MIN_BUDGET_REMAINING": "1"},
		{"MIN_BUDGET_REMAINING": "-0.1"},
		{"MIN_BUDGET_REMAINING": "NaN"},
		{"FAIL_MODE": "sometimes"},
	} {
		if _, err := ConfigFromEnv(env(bad)); err == nil {
			t.Errorf("ConfigFromEnv(%v): want an error", bad)
		}
	}
}
