package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
)

// Dispatch is a courier's connection to the dispatch service: the offer stream, and the
// accept and decline calls.
type Dispatch struct {
	endpoint
	// IdleTimeout is how long the offer stream may stay silent before we assume the
	// connection is dead and reconnect. The server pings every 15 s.
	IdleTimeout time.Duration
	// Reconnect is the backoff between reconnection attempts.
	Reconnect Retry
	logger    *slog.Logger
}

// NewDispatch returns a client for the dispatch service at baseURL.
func NewDispatch(baseURL string, httpClient *http.Client, observer Observer, logger *slog.Logger) *Dispatch {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Dispatch{
		endpoint:    newEndpoint(baseURL, httpClient, 5*time.Second, observer),
		IdleTimeout: 45 * time.Second, // three missed pings
		Reconnect:   Retry{Base: time.Second, Max: 30 * time.Second},
		logger:      logger,
	}
}

// OfferStore is the store in an offer.
type OfferStore struct {
	ID  string  `json:"id"`
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Offer asks a courier to take an order. It must be answered before ExpiresAt.
type Offer struct {
	ID          string     `json:"offer_id"`
	OrderID     string     `json:"order_id"`
	Store       OfferStore `json:"store"`
	Dropoff     geo.Point  `json:"dropoff"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ETAPickupS  int        `json:"eta_pickup_s"`
	ETADropoffS int        `json:"eta_dropoff_s"`
}

// OfferCancelled withdraws an offer, for example because the merchant cancelled the order.
type OfferCancelled struct {
	OfferID string `json:"offer_id"`
	Reason  string `json:"reason"`
}

// OfferEvent is one message from the offer stream: exactly one of the fields is set.
type OfferEvent struct {
	Offer     *Offer
	Cancelled *OfferCancelled
}

// ErrUnauthorized means the service rejected the courier's token (401).
var ErrUnauthorized = errors.New("courier token rejected")

// FollowOffers keeps the courier's offer stream open and sends every event to out. When the
// connection drops it reconnects with backoff, asking token for the current token each time
// (tokens are re-issued before they expire). It returns when ctx ends or a token is rejected.
func (d *Dispatch) FollowOffers(ctx context.Context, token func() string, out chan<- OfferEvent) error {
	failures := 0
	for {
		gotData, err := d.streamOnce(ctx, token(), out)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case StatusOf(err) == http.StatusUnauthorized:
			return ErrUnauthorized
		case gotData:
			failures = 0 // the connection worked for a while: start the backoff afresh
		}
		failures++
		d.logger.Debug("offer stream reconnecting", "err", err, "attempt", failures)
		if sleep(ctx, d.Reconnect.Delay(failures, nil)) != nil {
			return ctx.Err()
		}
	}
}

// streamOnce opens the offer stream and reads it until it fails. gotData reports whether the
// server sent anything, which separates a stream that dropped after a while from one that
// never worked.
func (d *Dispatch) streamOnce(ctx context.Context, token string, out chan<- OfferEvent) (gotData bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A silent connection is a dead one (a NAT or load balancer may have dropped it without
	// telling us). Every line from the server pushes this deadline back.
	watchdog := time.AfterFunc(d.IdleTimeout, cancel)
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/v1/couriers/me/offers/stream", nil)
	if err != nil {
		return false, fmt.Errorf("build offer stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Request-Id", newRequestID())

	start := time.Now()
	resp, err := d.http.Do(req)
	if err != nil {
		d.observer.ObserveRequest("offer_stream", Reason(err), time.Since(start))
		return false, fmt.Errorf("open offer stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	d.observer.ObserveRequest("offer_stream", strconv.Itoa(resp.StatusCode), time.Since(start))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, newAPIError(resp, body)
	}

	alive := func() {
		gotData = true
		watchdog.Reset(d.IdleTimeout)
	}
	err = readEvents(resp.Body, alive, func(ev sseEvent) error {
		msg, ok := d.parseEvent(ev)
		if !ok {
			return nil
		}
		select {
		case out <- msg:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return gotData, err
}

// parseEvent decodes an offer or offer_cancelled event. Unknown event names are ignored, so
// dispatch can add new events without breaking older couriers.
func (d *Dispatch) parseEvent(ev sseEvent) (OfferEvent, bool) {
	var err error
	switch ev.name {
	case "offer":
		var o Offer
		if err = json.Unmarshal([]byte(ev.data), &o); err == nil && o.ID != "" {
			return OfferEvent{Offer: &o}, true
		}
	case "offer_cancelled":
		var c OfferCancelled
		if err = json.Unmarshal([]byte(ev.data), &c); err == nil && c.OfferID != "" {
			return OfferEvent{Cancelled: &c}, true
		}
	default:
		return OfferEvent{}, false
	}
	d.logger.Warn("ignoring malformed offer stream event", "event", ev.name, "err", err)
	return OfferEvent{}, false
}

// Accept accepts an offer. A 409 means it expired or another courier took the order.
func (d *Dispatch) Accept(ctx context.Context, token, offerID string) error {
	return d.answer(ctx, token, offerID, "accept")
}

// Decline declines an offer, so dispatch can offer the order to someone else straight away.
func (d *Dispatch) Decline(ctx context.Context, token, offerID string) error {
	return d.answer(ctx, token, offerID, "decline")
}

func (d *Dispatch) answer(ctx context.Context, token, offerID, verb string) error {
	_, err := d.do(ctx, request{
		call: verb, method: http.MethodPost, token: token,
		path: "/v1/offers/" + url.PathEscape(offerID) + "/" + verb,
	}, nil)
	return err
}
