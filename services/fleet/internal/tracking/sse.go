package tracking

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/wards"
)

// pingFrame is the SSE comment sent every heartbeat. It keeps proxies from closing an idle
// connection and lets clients notice a dead one.
var pingFrame = []byte(": ping\n\n")

// StreamConfig tunes the HTTP side of the streams.
type StreamConfig struct {
	Heartbeat        time.Duration // TRACKING_HEARTBEAT
	WriteTimeout     time.Duration // TRACKING_WRITE_TIMEOUT: a client that stops reading is cut off
	UnknownOrderWait time.Duration // TRACKING_UNKNOWN_ORDER_WAIT: grace for a brand-new order's events
	FleetToken       string        // FLEET_STREAM_TOKEN: shared token for the ops console
	FleetPublic      bool          // FLEET_STREAM_PUBLIC: allow the fleet stream without a token (local only)
	CORSOrigins      []string      // TRACKING_CORS_ORIGINS: browser origins allowed to open streams
}

// Streams serves the two server-sent event endpoints.
type Streams struct {
	cfg      StreamConfig
	hub      *Hub
	orders   *Orders
	verifier *auth.Verifier
	stopping <-chan struct{} // closed when the server starts shutting down
	logger   *slog.Logger
	metrics  *metrics
	now      func() time.Time
}

// Register adds the stream routes to srv.
func (s *Streams) Register(srv *kit.Server) {
	srv.Handle("GET /v1/track/{token}/stream", s.cors(http.HandlerFunc(s.track)))
	srv.Handle("GET /v1/fleet/stream", s.cors(http.HandlerFunc(s.fleet)))
}

// track streams one order: its status, its courier's position, and "end" when it finishes.
func (s *Streams) track(w http.ResponseWriter, r *http.Request) {
	tok, err := s.verifier.Verify(r.PathValue("token"))
	if err != nil {
		kit.WriteError(w, http.StatusUnauthorized, "invalid_token", "the tracking link is invalid or has expired")
		return
	}
	if !s.waitForOrder(r.Context(), tok.Subject) {
		kit.WriteError(w, http.StatusNotFound, "order_not_found", "no order is known for this tracking link")
		return
	}
	// Subscribe before taking the snapshot: an update in between is then sent twice rather than
	// lost, and every status event carries the full state, so a repeat is harmless.
	v, err := s.hub.SubscribeOrder(tok.Subject)
	if err != nil {
		s.tooManyViewers(w)
		return
	}
	defer s.hub.Unsubscribe(v)
	s.serve(w, r, v, s.hub.OrderSnapshot(tok.Subject), tok.ExpiresAt)
}

// waitForOrder gives a brand-new order a moment to appear: a customer can open the tracking
// link before the OrderConfirmed event has reached this replica.
func (s *Streams) waitForOrder(ctx context.Context, orderID string) bool {
	deadline := time.NewTimer(s.cfg.UnknownOrderWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, ok := s.orders.Get(orderID); ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

// fleet streams every courier in a ward (or the whole city) to the ops console.
func (s *Streams) fleet(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.cfg.FleetToken != "":
		// Browsers' EventSource cannot set headers, so the token may also come as ?token=.
		// Request logs record the route pattern, never the query string.
		tok, ok := auth.BearerToken(r)
		if !ok {
			tok = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.FleetToken)) != 1 {
			kit.WriteError(w, http.StatusUnauthorized, "invalid_token", "a valid fleet stream token is required")
			return
		}
	case !s.cfg.FleetPublic:
		kit.WriteError(w, http.StatusForbidden, "fleet_stream_disabled",
			"set FLEET_STREAM_TOKEN (or FLEET_STREAM_PUBLIC=true for local runs) to enable this stream")
		return
	}
	ward := r.URL.Query().Get("ward")
	if ward != "" && !wards.Known(ward) {
		kit.WriteError(w, http.StatusBadRequest, "unknown_ward", "ward must be a ward ID from contracts/data/wards.json")
		return
	}
	v, err := s.hub.SubscribeFleet(ward)
	if err != nil {
		s.tooManyViewers(w)
		return
	}
	defer s.hub.Unsubscribe(v)
	s.serve(w, r, v, s.hub.FleetSnapshot(ward), time.Time{})
}

func (s *Streams) tooManyViewers(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	kit.WriteError(w, http.StatusServiceUnavailable, "too_many_viewers", "this replica is at its stream limit; retry shortly")
}

// serve writes the initial events, then live events, pings and finally "end" or nothing.
// It returns when the client leaves, a write fails, the token expires, the order finishes, or
// the server shuts down (the client's EventSource then reconnects to another replica).
func (s *Streams) serve(w http.ResponseWriter, r *http.Request, v *viewer, initial []message, expiresAt time.Time) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // tell nginx-style proxies not to buffer the stream
	w.WriteHeader(http.StatusOK)
	s.metrics.connections.WithLabelValues(v.stream).Inc()
	defer s.metrics.connections.WithLabelValues(v.stream).Dec()

	send := func(frame []byte) bool {
		// Per-write deadline instead of the server-wide WriteTimeout, which would end every
		// stream after 10 s. The error is ignored where deadlines are unsupported (tests).
		_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		if _, err := w.Write(frame); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send([]byte("retry: 3000\n\n")) { // EventSource reconnect delay, in milliseconds
		return
	}
	for _, m := range initial {
		if !send(m.frame) || m.final {
			return
		}
	}

	heartbeat := time.NewTicker(s.cfg.Heartbeat)
	defer heartbeat.Stop()
	var expired <-chan time.Time
	if !expiresAt.IsZero() {
		t := time.NewTimer(time.Until(expiresAt))
		defer t.Stop()
		expired = t.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopping:
			return
		case <-expired:
			send(endMessage("expired").frame)
			return
		case <-heartbeat.C:
			if !send(pingFrame) {
				return
			}
		case m := <-v.ch:
			if !m.receivedAt.IsZero() {
				// Tracking freshness SLI (design §9): age of a position when it reaches the viewer.
				s.metrics.eventAge.WithLabelValues(v.stream).Observe(s.now().Sub(m.receivedAt).Seconds())
			}
			if !send(m.frame) || m.final {
				return
			}
		}
	}
}

// cors lets the configured browser origins open streams. EventSource sends a simple GET, so no
// preflight handling is needed.
func (s *Streams) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" &&
			(slices.Contains(s.cfg.CORSOrigins, origin) || slices.Contains(s.cfg.CORSOrigins, "*")) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}
