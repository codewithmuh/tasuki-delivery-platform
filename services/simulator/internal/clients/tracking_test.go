package clients

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenFromURL(t *testing.T) {
	for url, want := range map[string]string{
		"http://localhost:8088/track/eyJhbGciOi.payload.sig": "eyJhbGciOi.payload.sig",
		"https://track.example.com/track/abc":                "abc",
		"https://track.example.com/orders/abc":               "",
		"https://track.example.com/track/":                   "",
		"https://track.example.com/track/abc/extra":          "",
		"https://track.example.com/track/abc?x=1":            "",
	} {
		got, ok := TokenFromURL(url)
		if got != want || ok != (want != "") {
			t.Errorf("TokenFromURL(%q) = %q, %v; want %q", url, got, ok, want)
		}
	}
}

// trackServer serves /v1/track/{token}/stream: connection n runs scripts[n-1].
func trackServer(t *testing.T, scripts ...func(w http.ResponseWriter, flush func())) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/track/tok_1/stream" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("got %s with Accept %q", r.URL.Path, r.Header.Get("Accept"))
		}
		n := int(conns.Add(1))
		if n > len(scripts) {
			<-r.Context().Done()
			return
		}
		scripts[n-1](w, w.(http.Flusher).Flush)
	}))
	t.Cleanup(srv.Close)
	return srv, &conns
}

func sse(w io.Writer, flush func(), event, data string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	flush()
}

func TestWatchFollowsAnOrderUntilTheServerEndsTheStream(t *testing.T) {
	srv, conns := trackServer(t,
		func(w http.ResponseWriter, flush func()) {
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w, flush, "status", `{"order_id": "ord_1", "status": "assigned", "eta": "2026-10-16T03:30:00Z"}`)
			sse(w, flush, "position", `{"courier_id": "cou_1", "lat": 35.66, "lng": 139.70, "received_at": "2026-10-16T03:12:08Z"}`)
			// the connection drops here; the viewer reconnects
		},
		func(w http.ResponseWriter, flush func()) {
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w, flush, "mystery", `{}`) // unknown events are ignored
			sse(w, flush, "status", `{"order_id": "ord_1", "status": "delivered", "eta": null}`)
			sse(w, flush, "end", `{"reason": "delivered"}`)
		},
	)
	tr := NewTracking(srv.URL, srv.Client(), nil)
	tr.Reconnect = Retry{Base: time.Millisecond, Max: time.Millisecond}

	var got []string
	err := tr.Watch(context.Background(), "tok_1", func(ev TrackEvent) {
		switch {
		case ev.Status != nil:
			got = append(got, "status:"+ev.Status.Status)
		case ev.Position != nil:
			got = append(got, fmt.Sprintf("position:%s@%s", ev.Position.CourierID, ev.Position.ReceivedAt.Format(time.TimeOnly)))
		case ev.End != "":
			got = append(got, "end:"+ev.End)
		}
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	want := "[status:assigned position:cou_1@03:12:08 status:delivered end:delivered]"
	if fmt.Sprint(got) != want {
		t.Errorf("events = %v\nwant     %s", got, want)
	}
	if conns.Load() != 2 {
		t.Errorf("%d connections, want 2 (one reconnect)", conns.Load())
	}
}

func TestWatchStopsOnAnExpiredLink(t *testing.T) {
	srv, conns := trackServer(t, func(w http.ResponseWriter, _ func()) {
		http.Error(w, `{"error": {"code": "invalid_token", "message": "expired"}}`, http.StatusUnauthorized)
	})
	tr := NewTracking(srv.URL, srv.Client(), nil)
	tr.Reconnect = Retry{Base: time.Millisecond, Max: time.Millisecond}
	err := tr.Watch(context.Background(), "tok_1", func(TrackEvent) {})
	if StatusOf(err) != http.StatusUnauthorized || conns.Load() != 1 {
		t.Fatalf("err = %v after %d connections; want a 401 and no retry", err, conns.Load())
	}
}

func TestWatchGivesUpAfterRepeatedFailures(t *testing.T) {
	fail := func(w http.ResponseWriter, _ func()) {
		http.Error(w, `{"error": {"code": "too_many_viewers", "message": "full"}}`, http.StatusServiceUnavailable)
	}
	srv, conns := trackServer(t, fail, fail, fail, fail)
	tr := NewTracking(srv.URL, srv.Client(), nil)
	tr.Reconnect = Retry{Base: time.Millisecond, Max: time.Millisecond}
	tr.MaxReconnects = 3
	err := tr.Watch(context.Background(), "tok_1", func(TrackEvent) {})
	if StatusOf(err) != http.StatusServiceUnavailable || conns.Load() != 4 {
		t.Fatalf("err = %v after %d connections; want a 503 after 1 try + 3 reconnects", err, conns.Load())
	}
}
