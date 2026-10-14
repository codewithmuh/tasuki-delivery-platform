// Package gate decides whether a service may start a canary, from the error budget that the
// Sloth-generated recording rules publish in Prometheus.
package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FailMode says what the gate answers when it cannot read the budget (Prometheus down, no data).
type FailMode string

// Fail modes. Closed is the default: an unknown budget is treated as a spent one.
const (
	FailClosed FailMode = "closed"
	FailOpen   FailMode = "open"
)

// Causes explain a decision in a stable, machine-readable way (metrics, alerts, logs).
const (
	CauseBudget          = "budget"
	CauseNoData          = "no_data"
	CausePrometheusError = "prometheus_error"
)

// ErrInvalidName is returned for a service or SLO name that is not a Sloth-style identifier.
var ErrInvalidName = errors.New("service (required) and slo (optional) must be 1-63 lowercase letters, digits or hyphens")

// Names are validated before they reach PromQL, which also rules out query injection.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// queryTimeout bounds one Prometheus query; Argo Rollouts' web provider waits 10 s by default.
const queryTimeout = 5 * time.Second

// Config holds the gate's settings, read from the environment (12-factor).
type Config struct {
	PrometheusURL string
	MinRemaining  float64 // block when the remaining budget ratio is at or below this
	FailMode      FailMode
}

// ConfigFromEnv builds a Config from environment lookups; getenv is os.Getenv in production.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{PrometheusURL: "http://localhost:9090", FailMode: FailClosed}
	if v := getenv("PROMETHEUS_URL"); v != "" {
		cfg.PrometheusURL = v
	}
	if v := getenv("MIN_BUDGET_REMAINING"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f >= 0 && f < 1) { // the negated form also rejects NaN
			return cfg, fmt.Errorf("MIN_BUDGET_REMAINING must be a ratio in [0, 1), got %q", v)
		}
		cfg.MinRemaining = f
	}
	switch m := FailMode(getenv("FAIL_MODE")); m {
	case "":
	case FailClosed, FailOpen:
		cfg.FailMode = m
	default:
		return cfg, fmt.Errorf("FAIL_MODE must be %q or %q, got %q", FailClosed, FailOpen, m)
	}
	return cfg, nil
}

// Decision is the gate's answer. Argo Rollouts reads it with `successCondition: result.allowed == true`.
type Decision struct {
	Service   string   `json:"service"`
	SLO       string   `json:"slo"` // the SLO asked for, or the one with the least budget when none was`
	Allowed   bool     `json:"allowed"`
	Remaining *float64 `json:"remaining"` // null when the budget is unknown
	Threshold float64  `json:"threshold"`
	Cause     string   `json:"cause"`
	Reason    string   `json:"reason"`
}

// Checker answers gate questions by querying Prometheus.
type Checker struct {
	cfg    Config
	client *http.Client
}

// NewChecker returns a Checker that uses client for Prometheus requests.
func NewChecker(cfg Config, client *http.Client) *Checker {
	return &Checker{cfg: cfg, client: client}
}

// Query is the PromQL for the remaining budget over the SLO period (28 days for Tasuki). An empty slo
// selects every SLO of the service. Validated names contain no quotes or backslashes, so %q produces
// a valid PromQL string.
func Query(service, slo string) string {
	if slo == "" {
		return fmt.Sprintf("slo:period_error_budget_remaining:ratio{sloth_service=%q}", service)
	}
	return fmt.Sprintf("slo:period_error_budget_remaining:ratio{sloth_service=%q,sloth_slo=%q}", service, slo)
}

// Check decides whether service may start a canary, judged on one SLO or, when slo is empty, on all of
// them (the error-budget policy freezes a service when any of its budgets is spent). It returns an
// error only for invalid input; Prometheus failures become a decision that follows the fail mode.
func (c *Checker) Check(ctx context.Context, service, slo string) (Decision, error) {
	if !namePattern.MatchString(service) || (slo != "" && !namePattern.MatchString(slo)) {
		return Decision{}, ErrInvalidName
	}
	d := Decision{Service: service, SLO: slo, Threshold: c.cfg.MinRemaining}
	remaining, lowestSLO, found, err := c.remaining(ctx, Query(service, slo))
	switch {
	case err != nil:
		return c.unknown(d, CausePrometheusError, err.Error()), nil
	case !found:
		return c.unknown(d, CauseNoData, "no error-budget series (Sloth rules loaded? any traffic yet?)"), nil
	}
	d.SLO, d.Remaining, d.Cause = lowestSLO, &remaining, CauseBudget
	d.Allowed = remaining > c.cfg.MinRemaining
	if d.Allowed {
		d.Reason = fmt.Sprintf("%s/%s: %.1f%% of the error budget remains", service, d.SLO, remaining*100)
	} else {
		d.Reason = fmt.Sprintf("%s/%s: error budget exhausted (%.1f%% remains, the gate needs more than %.1f%%); see docs/slo/error-budget-policy.md",
			service, d.SLO, remaining*100, c.cfg.MinRemaining*100)
	}
	return d, nil
}

func (c *Checker) unknown(d Decision, cause, why string) Decision {
	d.Cause = cause
	d.Allowed = c.cfg.FailMode == FailOpen
	d.Reason = fmt.Sprintf("budget unknown, failing %s: %s", c.cfg.FailMode, why)
	return d
}

// remaining runs an instant query and returns the lowest finite value and its SLO. Several series
// come back for a whole service (or from HA Prometheus pairs); the most pessimistic one wins.
// NaN means "no traffic in the window" and is skipped.
func (c *Checker) remaining(ctx context.Context, query string) (lowest float64, slo string, found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	endpoint := strings.TrimRight(c.cfg.PrometheusURL, "/") + "/api/v1/query?" + url.Values{"query": {query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, "", false, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, "", false, fmt.Errorf("query prometheus: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, "", false, fmt.Errorf("query prometheus: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"` // [unix time, "value as a string"]
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return 0, "", false, fmt.Errorf("decode prometheus response: %w", err)
	}
	if body.Status != "success" {
		return 0, "", false, fmt.Errorf("query prometheus: status %q", body.Status)
	}
	lowest = math.Inf(1)
	for _, r := range body.Data.Result {
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v >= lowest {
			continue
		}
		lowest, slo, found = v, r.Metric["sloth_slo"], true
	}
	return lowest, slo, found, nil
}
