package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Tracking is a customer's view of one delivery: the tracking service's stream for an order,
// the same one the tracking page reads.
type Tracking struct {
	endpoint
	// IdleTimeout is how long the stream may stay silent before we assume the connection is
	// dead and reconnect. The server pings every 15 s.
	IdleTimeout time.Duration
	// Reconnect is the backoff between reconnection attempts, and MaxReconnects how many
	// failed attempts in a row a viewer makes before giving up (a person closes the page).
	Reconnect     Retry
	MaxReconnects int
}

// NewTracking returns a client for the tracking service at baseURL.
func NewTracking(baseURL string, httpClient *http.Client, observer Observer) *Tracking {
	return &Tracking{
		endpoint:      newEndpoint(baseURL, httpClient, 5*time.Second, observer),
		IdleTimeout:   45 * time.Second, // three missed pings
		Reconnect:     Retry{Base: time.Second, Max: 30 * time.Second},
		MaxReconnects: 5,
	}
}

// TrackStatus is a `status` event: the order's state and its current ETA.
type TrackStatus struct {
	OrderID string     `json:"order_id"`
	Status  string     `json:"status"`
	ETA     *time.Time `json:"eta"`
}

// TrackPosition is a `position` event: where the assigned courier is, and when the platform
// received that point.
type TrackPosition struct {
	CourierID  string    `json:"courier_id"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	ReceivedAt time.Time `json:"received_at"`
}

// TrackEvent is one message from the stream: exactly one field is set. End holds the reason
// the server gave when it closed the stream (delivered, cancelled, failed or expired).
type TrackEvent struct {
	Status   *TrackStatus
	Position *TrackPosition
	End      string
}

// TokenFromURL returns the token at the end of an order's tracking_url
// ("<tracking page>/track/<token>"), or false if the URL does not have that shape.
func TokenFromURL(trackingURL string) (string, bool) {
	_, token, ok := strings.Cut(trackingURL, "/track/")
	if !ok || token == "" || strings.ContainsAny(token, "/?#") {
		return "", false
	}
	return token, true
}

// Watch follows one order until the server ends the stream, ctx ends, or the stream keeps
// failing. Like a browser's EventSource it reconnects with backoff, but it stops for good on
// 401 (the link expired) and 404 (unknown order). It returns nil when the server ended the
// stream normally.
func (t *Tracking) Watch(ctx context.Context, token string, handle func(TrackEvent)) error {
	failures := 0
	for {
		ended, gotData, err := t.streamOnce(ctx, token, handle)
		switch {
		case ended:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case StatusOf(err) == http.StatusUnauthorized, StatusOf(err) == http.StatusNotFound:
			return err
		case gotData:
			failures = 0 // the connection worked for a while: start the backoff afresh
		}
		failures++
		if failures > t.MaxReconnects {
			return err
		}
		if sleep(ctx, t.Reconnect.Delay(failures, err)) != nil {
			return ctx.Err()
		}
	}
}

// streamOnce opens the stream and reads it until it fails or the server sends `end`.
func (t *Tracking) streamOnce(ctx context.Context, token string, handle func(TrackEvent)) (ended, gotData bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(t.IdleTimeout, cancel)
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/v1/track/"+url.PathEscape(token)+"/stream", nil)
	if err != nil {
		return false, false, fmt.Errorf("build tracking stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-Request-Id", newRequestID())

	start := time.Now()
	resp, err := t.http.Do(req)
	if err != nil {
		t.observer.ObserveRequest("track_stream", Reason(err), time.Since(start))
		return false, false, fmt.Errorf("open tracking stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	t.observer.ObserveRequest("track_stream", strconv.Itoa(resp.StatusCode), time.Since(start))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, false, newAPIError(resp, body)
	}

	alive := func() {
		gotData = true
		watchdog.Reset(t.IdleTimeout)
	}
	err = readEvents(resp.Body, alive, func(ev sseEvent) error {
		msg, ok := parseTrackEvent(ev)
		if !ok {
			return nil
		}
		handle(msg)
		if msg.End != "" {
			ended = true
			return errStreamClosed
		}
		return nil
	})
	return ended, gotData, err
}

// parseTrackEvent decodes a status, position or end event. Unknown or malformed events are
// ignored, as the contract asks of clients.
func parseTrackEvent(ev sseEvent) (TrackEvent, bool) {
	switch ev.name {
	case "status":
		var s TrackStatus
		if json.Unmarshal([]byte(ev.data), &s) == nil && s.Status != "" {
			return TrackEvent{Status: &s}, true
		}
	case "position":
		var p TrackPosition
		if json.Unmarshal([]byte(ev.data), &p) == nil && !p.ReceivedAt.IsZero() {
			return TrackEvent{Position: &p}, true
		}
	case "end":
		var e struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(ev.data), &e) == nil {
			if e.Reason == "" {
				e.Reason = "unknown"
			}
			return TrackEvent{End: e.Reason}, true
		}
	}
	return TrackEvent{}, false
}
