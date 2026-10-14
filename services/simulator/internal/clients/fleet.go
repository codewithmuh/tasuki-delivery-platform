package clients

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// MaxBatch is the most GPS points fleet-ingest accepts in one request.
const MaxBatch = 50

// Fleet posts courier GPS points to fleet-ingest.
type Fleet struct {
	endpoint
}

// NewFleet returns a client for fleet-ingest at baseURL.
func NewFleet(baseURL string, httpClient *http.Client, observer Observer) *Fleet {
	return &Fleet{endpoint: newEndpoint(baseURL, httpClient, 3*time.Second, observer)}
}

// Location is one GPS point, as in the courier-location contract.
type Location struct {
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	AccuracyM  float64   `json:"accuracy_m"`
	SpeedMPS   float64   `json:"speed_mps"`
	HeadingDeg float64   `json:"heading_deg"`
	DeviceTime time.Time `json:"device_time"`
	Status     string    `json:"status"`             // idle, to_store, at_store, to_dropoff, offline
	OrderID    string    `json:"order_id,omitempty"` // omitted when the courier has no order
}

// LocationsResult says how many points fleet-ingest kept and how many it dropped as invalid.
type LocationsResult struct {
	Accepted int `json:"accepted"`
	Dropped  int `json:"dropped"`
}

// SendLocations posts a batch of 1–50 points. It never retries: a lost GPS point is harmless
// because the next one supersedes it. When fleet-ingest sheds load it answers 503 with
// Retry-After; see IsLoadShed.
func (f *Fleet) SendLocations(ctx context.Context, token string, points []Location) (LocationsResult, error) {
	var res LocationsResult
	_, err := f.do(ctx, request{
		call: "locations", method: http.MethodPost, path: "/v1/locations", token: token,
		body: map[string][]Location{"points": points},
	}, &res)
	return res, err
}

// IsLoadShed reports whether err is a 503 from a service shedding load, and how long the
// service asked callers to stay away.
func IsLoadShed(err error) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable {
		return apiErr.RetryAfter, true
	}
	return 0, false
}
