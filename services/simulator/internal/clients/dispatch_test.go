package clients

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadEvents(t *testing.T) {
	stream := ": ping\n\n" + // a comment on its own is not an event
		"event: offer\ndata: {\"a\":1}\n\n" +
		"event: offer\r\ndata:{\"b\":\r\ndata: 2}\r\n\r\n" + // CRLF, no space after the colon, two data lines
		"data: plain\n\n" + // no event name means "message"
		"event: lonely\n\n" + // a name without data is not dispatched
		"id: 7\nretry: 1000\nevent: offer\ndata: last\n\n" +
		"event: unterminated\ndata: never dispatched\n" // the stream ends mid-event

	var got []sseEvent
	lines := 0
	err := readEvents(strings.NewReader(stream), func() { lines++ }, func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if !errors.Is(err, errStreamClosed) {
		t.Fatalf("err = %v, want errStreamClosed", err)
	}
	want := []sseEvent{
		{"offer", `{"a":1}`}, {"offer", "{\"b\":\n2}"}, {"message", "plain"}, {"offer", "last"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q\nwant      %q", got, want)
	}
	if lines != 20 {
		t.Errorf("alive called %d times, want once per line (20)", lines)
	}

	stop := errors.New("stop")
	err = readEvents(strings.NewReader("data: x\n\ndata: y\n\n"), func() {}, func(sseEvent) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("handler error not returned: %v", err)
	}
}

// sseServer serves the offer stream: each connection gets the next script, a function that
// writes events and returns when the connection should end.
func sseServer(t *testing.T, scripts ...func(w io.Writer, flush func(), r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/couriers/me/offers/stream" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("got %s with Accept %q", r.URL.Path, r.Header.Get("Accept"))
		}
		n := int(conns.Add(1))
		if n > len(scripts) {
			<-r.Context().Done() // stay connected and silent
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		scripts[n-1](w, flusher.Flush, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &conns
}

const offerJSON = `{"offer_id": "off_1", "order_id": "ord_1", "store": {"id": "str_1", "lat": 35.66, "lng": 139.70},
"dropoff": {"lat": 35.67, "lng": 139.71}, "expires_at": "2026-10-14T03:00:30Z", "eta_pickup_s": 240, "eta_dropoff_s": 600}`

func TestFollowOffersDeliversEventsAndReconnects(t *testing.T) {
	srv, conns := sseServer(t,
		func(w io.Writer, flush func(), r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Errorf("first connection used %q", got)
			}
			_, _ = io.WriteString(w, ": ping\n\n")
			_, _ = io.WriteString(w, "event: offer\ndata: "+strings.ReplaceAll(offerJSON, "\n", " ")+"\n\n")
			_, _ = io.WriteString(w, "event: offer\ndata: {broken\n\n")   // ignored
			_, _ = io.WriteString(w, "event: surge_notice\ndata: {}\n\n") // unknown: ignored
			_, _ = io.WriteString(w, "event: offer_cancelled\ndata: {\"offer_id\": \"off_1\", \"reason\": \"order_cancelled\"}\n\n")
			flush()
			// returning closes the connection: the client must reconnect
		},
		func(w io.Writer, flush func(), r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer token-2" {
				t.Errorf("reconnection should use the current token, used %q", got)
			}
			_, _ = io.WriteString(w, "event: offer\ndata: {\"offer_id\": \"off_2\", \"order_id\": \"ord_2\"}\n\n")
			flush()
			<-r.Context().Done()
		},
	)

	d := NewDispatch(srv.URL, nil, nil, nil)
	d.Reconnect = Retry{Base: time.Millisecond, Max: 5 * time.Millisecond}
	var tokens atomic.Int32
	token := func() string { return fmt.Sprintf("token-%d", tokens.Add(1)) }

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan OfferEvent)
	done := make(chan error, 1)
	go func() { done <- d.FollowOffers(ctx, token, out) }()

	first := <-out
	if first.Offer == nil || first.Offer.ID != "off_1" || first.Offer.Store.ID != "str_1" || first.Offer.Dropoff.Lat != 35.67 ||
		first.Offer.ETAPickupS != 240 || !first.Offer.ExpiresAt.Equal(time.Date(2026, 10, 14, 3, 0, 30, 0, time.UTC)) {
		t.Fatalf("first event = %+v", first.Offer)
	}
	if second := <-out; second.Cancelled == nil || second.Cancelled.OfferID != "off_1" || second.Cancelled.Reason != "order_cancelled" {
		t.Fatalf("second event = %+v", second)
	}
	if third := <-out; third.Offer == nil || third.Offer.ID != "off_2" {
		t.Fatalf("third event = %+v", third)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("FollowOffers returned %v", err)
	}
	if conns.Load() != 2 {
		t.Errorf("connections = %d, want 2", conns.Load())
	}
}

func TestFollowOffersStopsWhenTheTokenIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusUnauthorized, "invalid_token")
	}))
	defer srv.Close()
	err := NewDispatch(srv.URL, nil, nil, nil).FollowOffers(context.Background(), func() string { return "expired" }, make(chan OfferEvent))
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestFollowOffersReconnectsASilentStream(t *testing.T) {
	// The first connection sends headers and then nothing, not even pings.
	srv, conns := sseServer(t, func(_ io.Writer, flush func(), r *http.Request) {
		flush()
		<-r.Context().Done()
	})
	d := NewDispatch(srv.URL, nil, nil, nil)
	d.IdleTimeout = 50 * time.Millisecond
	d.Reconnect = Retry{Base: time.Millisecond, Max: time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.FollowOffers(ctx, func() string { return "t" }, make(chan OfferEvent)) }()

	deadline := time.Now().Add(5 * time.Second)
	for conns.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the client never reconnected a silent stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFollowOffersBacksOffWhenUnreachable(t *testing.T) {
	d := NewDispatch("http://127.0.0.1:1", nil, nil, nil)
	d.Reconnect = Retry{Base: time.Millisecond, Max: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := d.FollowOffers(ctx, func() string { return "t" }, make(chan OfferEvent)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestAcceptAndDecline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/offers/off_1/accept":
			_, _ = io.WriteString(w, `{"offer_id": "off_1", "order_id": "ord_1", "status": "accepted"}`)
		case "/v1/offers/off_1/decline":
			_, _ = io.WriteString(w, `{"offer_id": "off_1", "order_id": "ord_1", "status": "declined"}`)
		case "/v1/offers/off_2/accept":
			writeError(w, http.StatusConflict, "offer_expired")
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := NewDispatch(srv.URL, nil, nil, nil)
	if err := d.Accept(context.Background(), "jwt", "off_1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Decline(context.Background(), "jwt", "off_1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Accept(context.Background(), "jwt", "off_2"); StatusOf(err) != http.StatusConflict {
		t.Fatalf("got %v", err)
	}
}
