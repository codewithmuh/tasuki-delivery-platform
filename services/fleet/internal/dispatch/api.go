package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/wards"
)

// etaCandidates is how many of the nearest free couriers GET /v1/eta asks OSRM about.
const etaCandidates = 5

// etaRedisOpenFor: after a Redis failure, GET /v1/eta answers 503 at once for this long instead
// of spending its whole budget on a Redis that is down. Game day GD-01 found every quote paying
// the full 250 ms while Valkey was gone; orders uses its own estimate on any 503 anyway.
const etaRedisOpenFor = 5 * time.Second

var pingFrame = []byte(": ping\n\n")

// APIConfig tunes the HTTP endpoints.
type APIConfig struct {
	Heartbeat         time.Duration // OFFER_HEARTBEAT: ": ping" and open-offer re-read interval
	WriteTimeout      time.Duration // OFFER_WRITE_TIMEOUT: a courier that stops reading is cut off
	SearchMaxRadiusM  float64       // SEARCH_MAX_RADIUS_M: supply is counted within this radius
	ETABudget         time.Duration // ETA_BUDGET: the whole GET /v1/eta, well inside orders' 300 ms
	ETARoutingTimeout time.Duration // ETA_ROUTING_TIMEOUT: OSRM's share of that budget
	SupplyLowBelow    int           // SUPPLY_LOW_BELOW: fewer free couriers nearby than this is "low"
}

// API serves the courier endpoints (offer stream, accept, decline) and the ETA endpoint. Any
// replica can serve any request: offers and reservations live in Redis.
type API struct {
	cfg      APIConfig
	store    *Store
	hub      *Hub
	outbox   *Outbox
	router   *Router
	verifier *auth.Verifier
	stopping <-chan struct{} // closed when shutdown begins: streams end so couriers reconnect elsewhere
	metrics  *metrics
	logger   *slog.Logger
	now      func() time.Time

	etaMu        sync.Mutex
	etaOpenUntil time.Time // GET /v1/eta skips Redis until then (etaRedisOpenFor)
}

// Register adds the routes to srv.
func (a *API) Register(srv *kit.Server) {
	srv.Handle("GET /v1/couriers/me/offers/stream", http.HandlerFunc(a.stream))
	srv.Handle("POST /v1/offers/{offer_id}/accept", http.HandlerFunc(a.accept))
	srv.Handle("POST /v1/offers/{offer_id}/decline", http.HandlerFunc(a.decline))
	srv.Handle("GET /v1/eta", http.HandlerFunc(a.eta))
}

// courier authenticates the request with a courier JWT and returns the courier ID from it (never
// from the request) and when the token expires.
func (a *API) courier(w http.ResponseWriter, r *http.Request) (string, time.Time, bool) {
	raw, ok := auth.BearerToken(r)
	if !ok {
		kit.WriteError(w, http.StatusUnauthorized, "unauthorized", "a courier token is required")
		return "", time.Time{}, false
	}
	tok, err := a.verifier.Verify(raw)
	if err != nil {
		kit.WriteError(w, http.StatusUnauthorized, "invalid_token", "the courier token is invalid or has expired")
		return "", time.Time{}, false
	}
	return tok.Subject, tok.ExpiresAt, true
}

// stream is GET /v1/couriers/me/offers/stream: the courier's offers as server-sent events.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	courierID, expiresAt, ok := a.courier(w, r)
	if !ok {
		return
	}
	// Register before reading the open offer: an offer made in between then arrives twice
	// (and is sent once, see "sent") rather than not at all.
	s, err := a.hub.Register(courierID)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		kit.WriteError(w, http.StatusServiceUnavailable, "too_many_streams", "this replica is at its stream limit; retry shortly")
		return
	}
	defer a.hub.Unregister(s)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // tell nginx-style proxies not to buffer the stream
	w.WriteHeader(http.StatusOK)
	send := func(frame []byte) bool {
		// A deadline per write instead of the server's WriteTimeout, which would end every stream.
		_ = rc.SetWriteDeadline(time.Now().Add(a.cfg.WriteTimeout))
		if _, err := w.Write(frame); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	sent := "" // the last offer sent on this stream, so pub/sub and re-reads never send it twice
	sendOpenOffer := func() bool {
		o, ok, err := a.store.OpenOffer(r.Context(), courierID, a.now())
		if err != nil || !ok || o.ID == sent {
			return true // nothing new (or Redis is down: the next heartbeat tries again)
		}
		sent = o.ID
		return send(message{Event: eventOffer, Data: []byte(o.Payload)}.frame())
	}
	if !send([]byte("retry: 3000\n\n")) || !sendOpenOffer() {
		return
	}

	heartbeat := time.NewTicker(a.cfg.Heartbeat)
	defer heartbeat.Stop()
	expired := time.NewTimer(time.Until(expiresAt))
	defer expired.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.stopping:
			return
		case <-expired.C:
			return // the courier app reconnects with a fresh token
		case <-heartbeat.C:
			// The re-read recovers an offer whose pub/sub message was lost (a Redis reconnect).
			if !send(pingFrame) || !sendOpenOffer() {
				return
			}
		case m := <-s.ch:
			if m.Event == eventOffer {
				if m.OfferID == sent {
					continue
				}
				sent = m.OfferID
			}
			if !send(m.frame()) {
				return
			}
		}
	}
}

type answerResponse struct {
	OfferID string `json:"offer_id"`
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

// accept is POST /v1/offers/{offer_id}/accept (empty body).
func (a *API) accept(w http.ResponseWriter, r *http.Request) {
	courierID, _, ok := a.courier(w, r)
	if !ok {
		return
	}
	offerID := r.PathValue("offer_id")
	if !events.ValidID(events.PrefixOffer, offerID) {
		kit.WriteError(w, http.StatusNotFound, "offer_not_found", "no such offer")
		return
	}
	now := a.now()
	ans, err := a.store.Accept(r.Context(), offerID, courierID, now)
	if err != nil {
		a.unavailable(w, "accept", err)
		return
	}
	switch ans.Result {
	case answerAccepted:
		a.outbox.Send(r.Context(), ans.Events)
		a.metrics.offers.WithLabelValues("accepted").Inc()
		if ans.FirstSLI {
			a.metrics.assignment.WithLabelValues(outcomeAssigned).Observe(now.Sub(ans.Offer.ConfirmedAt).Seconds())
		}
		a.logger.Info("courier assigned", "order_id", ans.Offer.OrderID, "offer_id", offerID, "courier_id", courierID,
			"ward", ans.Offer.Ward, "waited_s", int(now.Sub(ans.Offer.ConfirmedAt).Seconds()),
			"request_id", kit.RequestID(r.Context()))
		fallthrough
	case answerAlreadyAccepted:
		kit.WriteJSON(w, http.StatusOK, answerResponse{OfferID: offerID, OrderID: ans.Offer.OrderID, Status: offerAccepted})
	default:
		a.refuse(w, ans.Result)
	}
}

// decline is POST /v1/offers/{offer_id}/decline (empty body). The order goes back to the pool at
// once and its owner offers it to the next courier on its next pass.
func (a *API) decline(w http.ResponseWriter, r *http.Request) {
	courierID, _, ok := a.courier(w, r)
	if !ok {
		return
	}
	offerID := r.PathValue("offer_id")
	if !events.ValidID(events.PrefixOffer, offerID) {
		kit.WriteError(w, http.StatusNotFound, "offer_not_found", "no such offer")
		return
	}
	now := a.now()
	ans, err := a.store.Decline(r.Context(), offerID, courierID, now, now)
	if err != nil {
		a.unavailable(w, "decline", err)
		return
	}
	switch ans.Result {
	case answerDeclined:
		a.outbox.Send(r.Context(), ans.Events)
		a.metrics.offers.WithLabelValues("declined").Inc()
		fallthrough
	case answerAlreadyDeclined:
		kit.WriteJSON(w, http.StatusOK, answerResponse{OfferID: offerID, OrderID: ans.Offer.OrderID, Status: offerDeclined})
	default:
		a.refuse(w, ans.Result)
	}
}

// refuse answers an accept or decline that cannot be applied. 409 tells the courier app the offer
// is gone (expired, taken, withdrawn, answered the other way, or not theirs).
func (a *API) refuse(w http.ResponseWriter, result string) {
	switch result {
	case answerNotFound:
		kit.WriteError(w, http.StatusNotFound, "offer_not_found", "no such offer")
	case answerNotYours:
		kit.WriteError(w, http.StatusConflict, "offer_not_yours", "this offer was made to another courier")
	case answerExpired:
		kit.WriteError(w, http.StatusConflict, "offer_expired", "the offer has expired")
	case answerCourierBusy:
		kit.WriteError(w, http.StatusConflict, "courier_busy", "finish your current order first")
	default:
		kit.WriteError(w, http.StatusConflict, "offer_closed", "the offer is no longer open ("+result+")")
	}
}

func (a *API) unavailable(w http.ResponseWriter, what string, err error) {
	a.logger.Warn("request failed", "what", what, "err", err)
	w.Header().Set("Retry-After", "1")
	kit.WriteError(w, http.StatusServiceUnavailable, "unavailable", "dispatch state is unavailable; retry shortly")
}

type etaResponse struct {
	EtaSeconds int    `json:"eta_seconds"`
	PickupS    int    `json:"pickup_s"`
	TravelS    int    `json:"travel_s"`
	Supply     string `json:"supply"`
}

// Supply levels in GET /v1/eta.
const (
	supplyOK   = "ok"
	supplyLow  = "low"
	supplyNone = "none"
)

// eta is GET /v1/eta?ward=&store_lat=&store_lng=&dropoff_lat=&dropoff_lng=, called by orders for
// every quote with a 300 ms timeout. It answers within ETA_BUDGET: OSRM gets ETA_ROUTING_TIMEOUT
// and the straight-line estimate fills in after that.
//
// pickup_s is the road time of the nearest free courier to the store. With no free courier within
// SEARCH_MAX_RADIUS_M it is the time to ride in from that radius, so quotes stay honest in a surge.
func (a *API) eta(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if ward := q.Get("ward"); ward != "" && !wards.Known(ward) {
		kit.WriteError(w, http.StatusBadRequest, "unknown_ward", "ward must be a ward ID from contracts/data/wards.json")
		return
	}
	store, err1 := pointParam(q, "store_lat", "store_lng")
	dropoff, err2 := pointParam(q, "dropoff_lat", "dropoff_lng")
	if err := errors.Join(err1, err2); err != nil {
		kit.WriteError(w, http.StatusBadRequest, "invalid_query",
			"store_lat, store_lng, dropoff_lat and dropoff_lng must be valid coordinates")
		return
	}
	if a.etaRedisDown() {
		w.Header().Set("Retry-After", "1")
		kit.WriteError(w, http.StatusServiceUnavailable, "unavailable", "dispatch state is unavailable; retry shortly")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.ETABudget)
	defer cancel()
	free, err := a.freeCouriers(ctx, store)
	if err != nil {
		if r.Context().Err() == nil { // Redis failed or was too slow, not the caller hanging up
			a.etaMu.Lock()
			a.etaOpenUntil = a.now().Add(etaRedisOpenFor)
			a.etaMu.Unlock()
		}
		a.unavailable(w, "eta", err)
		return
	}
	supply := supplyOK
	switch {
	case len(free) == 0:
		supply = supplyNone
	case len(free) < a.cfg.SupplyLowBelow:
		supply = supplyLow
	}

	nearest := free[:min(len(free), etaCandidates)]
	sources := make([]Point, 0, len(nearest)+1)
	for _, c := range nearest {
		sources = append(sources, c.Point)
	}
	sources = append(sources, store)
	d, _ := a.router.Durations(ctx, sources, []Point{store, dropoff}, a.cfg.ETARoutingTimeout)
	travel := d[len(nearest)][1]
	pickup := FallbackSeconds(a.cfg.SearchMaxRadiusM)
	for i := range nearest {
		pickup = min(pickup, d[i][0])
	}
	kit.WriteJSON(w, http.StatusOK, etaResponse{EtaSeconds: pickup + travel, PickupS: pickup, TravelS: travel, Supply: supply})
}

// etaRedisDown reports whether a recent Redis failure means GET /v1/eta should not try Redis.
func (a *API) etaRedisDown() bool {
	a.etaMu.Lock()
	defer a.etaMu.Unlock()
	return a.now().Before(a.etaOpenUntil)
}

// freeCouriers returns the couriers near p that hold no offer and carry no order, nearest first.
func (a *API) freeCouriers(ctx context.Context, p Point) ([]Nearby, error) {
	found, err := a.store.Nearby(ctx, p, a.cfg.SearchMaxRadiusM, 3*etaCandidates)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(found))
	for i, f := range found {
		ids[i] = f.CourierID
	}
	engaged, err := a.store.Engaged(ctx, ids)
	if err != nil {
		return nil, err
	}
	free := found[:0]
	for _, f := range found {
		if !engaged[f.CourierID] {
			free = append(free, f)
		}
	}
	return free, nil
}

func pointParam(q url.Values, latKey, lngKey string) (Point, error) {
	lat, err1 := strconv.ParseFloat(q.Get(latKey), 64)
	lng, err2 := strconv.ParseFloat(q.Get(lngKey), 64)
	if err := errors.Join(err1, err2); err != nil {
		return Point{}, err
	}
	if !usable(lat, lng) || math.IsInf(lat, 0) || math.IsInf(lng, 0) {
		return Point{}, errors.New("coordinates out of range")
	}
	return Point{Lat: lat, Lng: lng}, nil
}
