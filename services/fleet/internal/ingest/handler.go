// Package ingest implements fleet-ingest: the authenticated endpoint where courier apps send
// batches of GPS points (design §7.2 step 2). It validates each point, stamps received_at, and
// publishes the batch to Kafka. When Kafka is slow or down it sheds load with 503 + Retry-After;
// losing a batch is acceptable here because the courier app sends a fresher one seconds later.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// Config holds fleet-ingest's own settings.
type Config struct {
	Topic              string        // KAFKA_TOPIC_LOCATIONS
	MaxPoints          int           // INGEST_MAX_POINTS: points per batch (contract: 1-50)
	MaxBodyBytes       int64         // INGEST_MAX_BODY_BYTES: larger bodies get 413
	MaxFutureSkew      time.Duration // INGEST_MAX_FUTURE_SKEW: drop points stamped further in the future
	PublishTimeout     time.Duration // INGEST_PUBLISH_TIMEOUT: wait this long for Kafka, then shed
	RetryAfter         time.Duration // INGEST_RETRY_AFTER: what a 503 tells the courier app
	MaxBufferedRecords int           // INGEST_MAX_BUFFERED_RECORDS: producer buffer; full means shed
}

// LoadConfig reads fleet-ingest's settings.
func LoadConfig(e *kit.Env) Config {
	return Config{
		Topic:              e.String("KAFKA_TOPIC_LOCATIONS", events.TopicCourierLocations),
		MaxPoints:          e.Int("INGEST_MAX_POINTS", 50, 1),
		MaxBodyBytes:       int64(e.Int("INGEST_MAX_BODY_BYTES", 64<<10, 1024)),
		MaxFutureSkew:      e.Duration("INGEST_MAX_FUTURE_SKEW", 30*time.Second),
		PublishTimeout:     e.Duration("INGEST_PUBLISH_TIMEOUT", 2*time.Second),
		RetryAfter:         e.Duration("INGEST_RETRY_AFTER", 5*time.Second),
		MaxBufferedRecords: e.Int("INGEST_MAX_BUFFERED_RECORDS", 10000, 1),
	}
}

// point is one GPS reading as the courier app sends it. lat and lng are pointers because a
// missing coordinate must be rejected, not read as 0 (a point in the Gulf of Guinea).
type point struct {
	Lat        *float64             `json:"lat"`
	Lng        *float64             `json:"lng"`
	AccuracyM  *float64             `json:"accuracy_m"`
	SpeedMPS   *float64             `json:"speed_mps"`
	HeadingDeg *float64             `json:"heading_deg"`
	DeviceTime time.Time            `json:"device_time"`
	Status     events.CourierStatus `json:"status"`
	OrderID    *string              `json:"order_id"`
}

// batch keeps points raw so that one malformed point is dropped on its own instead of failing
// the whole request.
type batch struct {
	Points []json.RawMessage `json:"points"`
}

// Point outcomes, used as the "result" label of fleet_ingest_points_total.
const (
	resultAccepted  = "accepted"
	resultInvalid   = "invalid"
	resultFuture    = "future"
	resultDuplicate = "duplicate"
	resultShed      = "shed"
)

// Handler serves POST /v1/locations.
type Handler struct {
	cfg      Config
	verifier *auth.Verifier
	pub      Publisher
	logger   *slog.Logger
	now      func() time.Time
	points   *prometheus.CounterVec
	shed     *prometheus.CounterVec
	batch    prometheus.Histogram
}

// NewHandler builds the handler and registers its metrics on reg.
func NewHandler(cfg Config, verifier *auth.Verifier, pub Publisher, logger *slog.Logger, reg prometheus.Registerer) *Handler {
	h := &Handler{
		cfg: cfg, verifier: verifier, pub: pub, logger: logger, now: time.Now,
		points: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_ingest_points_total",
			Help: "GPS points received, by result: accepted, invalid, future (device_time too far ahead), duplicate, shed.",
		}, []string{"result"}),
		shed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_ingest_shed_requests_total",
			Help: "Batches refused with 503 because Kafka could not take them, by reason: buffer_full, timeout, kafka_error.",
		}, []string{"reason"}),
		batch: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "fleet_ingest_batch_points",
			Help:    "Points per accepted request (1 normally; more after a courier app was offline).",
			Buckets: []float64{1, 2, 5, 10, 20, 50},
		}),
	}
	reg.MustRegister(h.points, h.shed, h.batch)
	for _, r := range []string{resultAccepted, resultInvalid, resultFuture, resultDuplicate, resultShed} {
		h.points.WithLabelValues(r) // export zeros, so rate() works before the first event
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, ok := auth.BearerToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		kit.WriteError(w, http.StatusUnauthorized, "unauthorized", "a courier token is required")
		return
	}
	tok, err := h.verifier.Verify(raw)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		kit.WriteError(w, http.StatusUnauthorized, "invalid_token", "the courier token is invalid or expired")
		return
	}
	receivedAt := h.now().UTC() // one stamp for the whole batch: the moment we accepted it

	var b batch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)).Decode(&b); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			kit.WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "the request body is too large")
			return
		}
		kit.WriteError(w, http.StatusBadRequest, "invalid_json", "the body must be JSON: {\"points\": [...]}")
		return
	}
	if len(b.Points) == 0 || len(b.Points) > h.cfg.MaxPoints {
		kit.WriteError(w, http.StatusUnprocessableEntity, "invalid_batch",
			"a batch must contain between 1 and "+strconv.Itoa(h.cfg.MaxPoints)+" points")
		return
	}

	locs, counts := h.validate(tok.Subject, receivedAt, b.Points)
	dropped := len(b.Points) - len(locs)
	if counts[resultInvalid] == len(b.Points) {
		h.points.WithLabelValues(resultInvalid).Add(float64(dropped))
		kit.WriteError(w, http.StatusUnprocessableEntity, "invalid_points", "every point in the batch is invalid")
		return
	}

	if len(locs) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), h.cfg.PublishTimeout)
		err := h.pub.Publish(ctx, locs)
		cancel()
		if err != nil {
			h.shedLoad(w, r, err, len(locs), counts)
			return
		}
	}
	counts[resultAccepted] = len(locs)
	h.count(counts)
	h.batch.Observe(float64(len(locs)))
	kit.WriteJSON(w, http.StatusAccepted, map[string]int{"accepted": len(locs), "dropped": dropped})
}

// validate turns raw points into events and sorts out the ones to drop.
func (h *Handler) validate(courierID string, receivedAt time.Time, raws []json.RawMessage) ([]events.CourierLocation, map[string]int) {
	counts := map[string]int{}
	locs := make([]events.CourierLocation, 0, len(raws))
	seen := make(map[int64]bool, len(raws))
	latestAllowed := receivedAt.Add(h.cfg.MaxFutureSkew)
	for _, raw := range raws {
		var p point
		if err := json.Unmarshal(raw, &p); err != nil || p.Lat == nil || p.Lng == nil {
			counts[resultInvalid]++
			continue
		}
		loc := events.CourierLocation{
			Schema:     events.SchemaCourierLocation,
			EventID:    events.NewID(events.PrefixEvent),
			CourierID:  courierID, // from the token, never from the body
			Lat:        *p.Lat,
			Lng:        *p.Lng,
			AccuracyM:  p.AccuracyM,
			SpeedMPS:   p.SpeedMPS,
			HeadingDeg: p.HeadingDeg,
			DeviceTime: p.DeviceTime.UTC(),
			ReceivedAt: receivedAt,
			Status:     p.Status,
			OrderID:    p.OrderID,
		}
		switch {
		case loc.Validate() != nil:
			counts[resultInvalid]++
		case loc.DeviceTime.After(latestAllowed):
			// A clock far ahead would make this point "newest" for too long and freeze the
			// courier on the map, because consumers keep the newest point by device_time.
			counts[resultFuture]++
		case seen[loc.DeviceTime.UnixNano()]:
			counts[resultDuplicate]++
		default:
			seen[loc.DeviceTime.UnixNano()] = true
			locs = append(locs, loc)
		}
	}
	return locs, counts
}

func (h *Handler) shedLoad(w http.ResponseWriter, r *http.Request, err error, points int, counts map[string]int) {
	reason := "kafka_error"
	switch {
	case errors.Is(err, ErrOverloaded):
		reason = "buffer_full"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "timeout"
	}
	counts[resultShed] = points
	h.count(counts)
	h.shed.WithLabelValues(reason).Inc()
	h.logger.Warn("shedding load", "request_id", kit.RequestID(r.Context()), "reason", reason, "err", err)
	// Retry-After is in whole seconds (RFC 9110); round up, and never tell a client to retry at once.
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(h.cfg.RetryAfter.Seconds())))))
	kit.WriteError(w, http.StatusServiceUnavailable, "overloaded", "location ingestion is shedding load; retry later")
}

func (h *Handler) count(counts map[string]int) {
	for result, n := range counts {
		if n > 0 {
			h.points.WithLabelValues(result).Add(float64(n))
		}
	}
}
