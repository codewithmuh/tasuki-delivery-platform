// Package clients holds small typed HTTP clients for the Tasuki APIs the simulator calls:
// orders, fleet-ingest, dispatch (including its server-sent event stream) and OSRM routing.
// They use only the public HTTP contracts in contracts/README.md, like a real app would.
package clients

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Observer receives a call for every HTTP attempt, for metrics. *metrics.Metrics implements it.
type Observer interface {
	ObserveRequest(call, outcome string, d time.Duration)
	Retried(call string)
}

type noObserver struct{}

func (noObserver) ObserveRequest(string, string, time.Duration) {}
func (noObserver) Retried(string)                               {}

// NewHTTPClient returns an HTTP client sized for thousands of simulated phones talking to a
// handful of services. It has no overall timeout: each call sets its own through its context,
// and the offer streams must stay open indefinitely.
func NewHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 1024
	// The default of 2 idle connections per host would make most GPS batches open (and later
	// close) a new TCP connection when hundreds of couriers post at once.
	t.MaxIdleConnsPerHost = 512
	return &http.Client{Transport: t}
}

// APIError is a non-2xx answer from a service. Tasuki services describe the problem in the
// body as {"error": {"code": "...", "message": "..."}}.
type APIError struct {
	Status     int
	Code       string        // e.g. "quote_expired"; empty if the body had no error object
	Message    string        // the server's message, or the start of the body
	RetryAfter time.Duration // from the Retry-After header; 0 if absent
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// errBadResponse marks a 2xx answer whose body could not be decoded.
var errBadResponse = errors.New("bad response body")

// StatusOf returns the HTTP status code behind err, or 0 if the server never answered.
func StatusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// CodeOf returns the error code the server gave (for example "quote_expired"), or "".
func CodeOf(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// Reason returns a short label for why a call failed, safe to use as a metric label:
// "timeout", "network", "cancelled", "bad_response" or "http_<status>".
func Reason(err error) string {
	var netErr net.Error
	switch {
	case err == nil:
		return "ok"
	case StatusOf(err) != 0:
		return "http_" + strconv.Itoa(StatusOf(err))
	case errors.Is(err, errBadResponse):
		return "bad_response"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "network"
	}
}

// Retryable reports whether a failed call may succeed if it is repeated: timeouts, network
// errors, 429 Too Many Requests and 5xx. A 4xx means the request itself is wrong (or already
// done), so repeating it cannot help.
func Retryable(err error) bool {
	switch status := StatusOf(err); {
	case status == http.StatusTooManyRequests || status >= 500:
		return true
	case status != 0:
		return false
	}
	reason := Reason(err)
	return reason == "timeout" || reason == "network"
}

// Retry is how often, and how patiently, a call is retried.
type Retry struct {
	Attempts int           // total attempts, including the first
	Base     time.Duration // the backoff before the first retry; it doubles each time
	Max      time.Duration // the longest backoff
}

// maxRetryAfter caps how long a server's Retry-After can make us wait.
const maxRetryAfter = time.Minute

// Delay returns how long to wait before retry number n (1 for the first retry) after err.
// A server's Retry-After wins. Otherwise it is exponential backoff with "full jitter": a random
// wait between 0 and the backoff, so clients that failed together do not retry together.
func (p Retry) Delay(n int, err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return min(apiErr.RetryAfter, maxRetryAfter)
	}
	backoff := p.Max
	if shift := n - 1; shift < 63 && p.Base <= p.Max>>shift {
		backoff = p.Base << shift // Base × 2^shift; cannot overflow because it is at most Max
	}
	if backoff <= 0 {
		return 0
	}
	return time.Duration(mathrand.Int64N(int64(backoff))) + 1 //nolint:gosec // jitter, not security
}

// endpoint is what every client shares: where the service is and how to call it.
type endpoint struct {
	baseURL  string
	http     *http.Client
	timeout  time.Duration // per attempt
	observer Observer
}

func newEndpoint(baseURL string, httpClient *http.Client, timeout time.Duration, observer Observer) endpoint {
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	if observer == nil {
		observer = noObserver{}
	}
	return endpoint{baseURL: baseURL, http: httpClient, timeout: timeout, observer: observer}
}

// request describes one HTTP call.
type request struct {
	call   string // metric label, e.g. "quote"
	method string
	path   string
	token  string            // bearer token, if any
	header map[string]string // extra headers, e.g. Idempotency-Key
	body   any               // sent as JSON when not nil
}

// do makes one attempt. A 2xx JSON body is decoded into out (when out is not nil); anything
// else becomes an *APIError. It returns the response headers on success.
func (e *endpoint) do(ctx context.Context, r request, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	var body io.Reader
	if r.body != nil {
		data, err := json.Marshal(r.body)
		if err != nil {
			return nil, fmt.Errorf("encode %s request: %w", r.call, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, e.baseURL+r.path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", r.call, err)
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// The request ID lets a failure seen here be found in the service's own logs.
	req.Header.Set("X-Request-Id", newRequestID())
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := e.http.Do(req)
	if err != nil {
		err = fmt.Errorf("%s %s: %w", r.method, r.path, err)
		e.observer.ObserveRequest(r.call, Reason(err), time.Since(start))
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	err = readResponse(resp, out)
	e.observer.ObserveRequest(r.call, strconv.Itoa(resp.StatusCode), time.Since(start))
	if err != nil {
		return nil, err
	}
	return resp.Header, nil
}

// withRetry calls attempt until it succeeds, fails in a way that retrying cannot fix, runs out
// of attempts, or ctx ends.
func (e *endpoint) withRetry(ctx context.Context, call string, policy Retry, attempt func() error) error {
	var err error
	for n := range max(policy.Attempts, 1) {
		if n > 0 {
			e.observer.Retried(call)
			if sleepErr := sleep(ctx, policy.Delay(n, err)); sleepErr != nil {
				return err // ctx ended while waiting: report the last real failure
			}
		}
		err = attempt()
		if err == nil || !Retryable(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// readResponse decodes a 2xx body into out, or turns any other answer into an *APIError.
func readResponse(resp *http.Response, out any) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: %w", errBadResponse, err)
	}
	return nil
}

func newAPIError(resp *http.Response, body []byte) *APIError {
	apiErr := &APIError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Code != "" {
		apiErr.Code, apiErr.Message = envelope.Error.Code, envelope.Error.Message
		return apiErr
	}
	// Not the Tasuki error shape (OSRM, a proxy, ...): keep the start of the body.
	apiErr.Message = string(body[:min(len(body), 200)])
	return apiErr
}

// parseRetryAfter reads a Retry-After header: a number of seconds or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}

// sleep waits for d or until ctx ends, whichever is first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
