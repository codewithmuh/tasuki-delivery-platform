package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The fallback road-time estimate, the same one orders uses (services/orders/src/orders/eta.py):
// straight-line distance times a detour factor, at bicycle speed.
const (
	DetourFactor      = 1.35               // road distance / straight-line distance, a common figure for dense cities
	FallbackSpeedMPS  = 15 * 1000 / 3600.0 // about 15 km/h: a bicycle or scooter in city traffic
	earthRadiusMetres = 6371000.0
)

// Breaker settings: after breakerFailures OSRM failures in a row, skip OSRM for breakerOpenFor
// and use the fallback at once, so a slow OSRM cannot add its timeout to every match and quote.
const (
	breakerFailures = 3
	breakerOpenFor  = 10 * time.Second
)

// HaversineMetres is the great-circle distance between two points.
func HaversineMetres(a, b Point) float64 {
	rad := math.Pi / 180
	dLat, dLng := (b.Lat-a.Lat)*rad, (b.Lng-a.Lng)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusMetres * math.Asin(math.Min(1, math.Sqrt(h)))
}

// FallbackSeconds is the estimated riding time for a straight-line distance.
func FallbackSeconds(distanceM float64) int {
	return int(math.Ceil(distanceM * DetourFactor / FallbackSpeedMPS))
}

// Router answers "how long by road" with OSRM's table service, and falls back to the straight-line
// estimate when OSRM is disabled, failing or slow.
type Router struct {
	baseURL string // "" = OSRM disabled (ROUTING_URL=none)
	client  *http.Client
	metrics *metrics
	now     func() time.Time

	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

// NewRouter returns a Router for the OSRM server at baseURL; "" disables OSRM.
func NewRouter(baseURL string, m *metrics) *Router {
	return &Router{
		baseURL: strings.TrimRight(baseURL, "/"),
		// The per-call context sets the real deadline; this only bounds a call made without one.
		client:  &http.Client{Timeout: 5 * time.Second},
		metrics: m,
		now:     time.Now,
	}
}

// Durations returns travel times in seconds from every source to every destination, as
// result[source][destination]. It makes one OSRM table call within timeout. A pair OSRM cannot
// route, or every pair when OSRM fails, gets the fallback estimate; source says which one was
// used for the call. It never fails: an estimate is always better than no answer.
func (r *Router) Durations(ctx context.Context, sources, destinations []Point, timeout time.Duration) ([][]int, string) {
	out := make([][]int, len(sources))
	for i, s := range sources {
		out[i] = make([]int, len(destinations))
		for j, d := range destinations {
			out[i][j] = FallbackSeconds(HaversineMetres(s, d))
		}
	}
	if len(sources) == 0 || len(destinations) == 0 {
		return out, sourceFallback
	}
	if !r.allow() {
		r.metrics.etaRequests.WithLabelValues(sourceFallback).Inc()
		return out, sourceFallback
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	table, err := r.table(ctx, sources, destinations)
	r.record(err)
	if err != nil {
		r.metrics.etaRequests.WithLabelValues(sourceFallback).Inc()
		return out, sourceFallback
	}
	for i := range out {
		for j := range out[i] {
			if v := table[i][j]; v != nil && *v >= 0 {
				out[i][j] = int(math.Round(*v))
			}
		}
	}
	r.metrics.etaRequests.WithLabelValues(sourceOSRM).Inc()
	return out, sourceOSRM
}

// table calls GET /table/v1/bicycle/{lng,lat;...}?sources=...&destinations=... Sources and
// destinations are listed once each in the coordinate list.
func (r *Router) table(ctx context.Context, sources, destinations []Point) ([][]*float64, error) {
	coords := make([]string, 0, len(sources)+len(destinations))
	src := make([]string, len(sources))
	dst := make([]string, len(destinations))
	for i, p := range sources {
		src[i] = strconv.Itoa(len(coords))
		coords = append(coords, lngLat(p))
	}
	for i, p := range destinations {
		dst[i] = strconv.Itoa(len(coords))
		coords = append(coords, lngLat(p))
	}
	url := r.baseURL + "/table/v1/bicycle/" + strings.Join(coords, ";") +
		"?sources=" + strings.Join(src, ";") + "&destinations=" + strings.Join(dst, ";") + "&annotations=duration"
	// The host is ROUTING_URL from configuration, and the path holds only numbers formatted
	// above, so a caller of GET /v1/eta cannot steer this request anywhere else.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: see above
	if err != nil {
		return nil, fmt.Errorf("osrm request: %w", err)
	}
	resp, err := r.client.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return nil, fmt.Errorf("osrm table: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Code      string       `json:"code"`
		Durations [][]*float64 `json:"durations"` // null where OSRM found no route
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("osrm table: status %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || body.Code != "Ok" {
		return nil, fmt.Errorf("osrm table: status %d, code %q", resp.StatusCode, body.Code)
	}
	if len(body.Durations) != len(sources) {
		return nil, fmt.Errorf("osrm table: %d rows for %d sources", len(body.Durations), len(sources))
	}
	for _, row := range body.Durations {
		if len(row) != len(destinations) {
			return nil, fmt.Errorf("osrm table: %d columns for %d destinations", len(row), len(destinations))
		}
	}
	return body.Durations, nil
}

// lngLat formats a point the way OSRM wants it: longitude first.
func lngLat(p Point) string {
	return strconv.FormatFloat(p.Lng, 'f', 6, 64) + "," + strconv.FormatFloat(p.Lat, 'f', 6, 64)
}

// allow reports whether OSRM may be called: it is configured and the breaker is closed.
func (r *Router) allow() bool {
	if r.baseURL == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.now().Before(r.openUntil)
}

// record counts consecutive failures. After breakerFailures of them OSRM is skipped for
// breakerOpenFor; the first call after that is a trial, and one more failure opens it again.
func (r *Router) record(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.failures = 0
		return
	}
	r.failures++
	if r.failures >= breakerFailures {
		r.openUntil = r.now().Add(breakerOpenFor)
		r.failures = breakerFailures - 1
	}
}
