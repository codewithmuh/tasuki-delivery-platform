package sim

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
)

// fakeTasuki is a tiny in-memory stand-in for orders, dispatch, fleet-ingest and OSRM, just
// faithful enough to the contracts for the simulator to complete deliveries against it.
type fakeTasuki struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	merchants   int
	couriers    int
	reissued    int
	tokenTTL    time.Duration
	courierOf   map[string]string // token → courier ID
	quotes      map[string]geo.Point
	idempotency map[string]string // Idempotency-Key → order ID
	dropoffs    map[string]geo.Point
	orders      int
	replays     int
	delivered   int
	badProofs   int
	points      int
	routes      int
	streams     map[string]chan string // courier ID → events to send
	order       []string               // courier IDs with an open stream, in connection order
	next        int                    // round-robin position for offers
	offers      map[string]string      // offer ID → order ID
	offerSeq    int
}

func newFakeTasuki(t *testing.T) *fakeTasuki {
	f := &fakeTasuki{
		t: t, tokenTTL: 24 * time.Hour,
		courierOf: map[string]string{}, quotes: map[string]geo.Point{}, idempotency: map[string]string{},
		dropoffs: map[string]geo.Point{}, streams: map[string]chan string{}, offers: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/admin/merchants", f.createMerchant)
	mux.HandleFunc("POST /v1/admin/couriers", f.createCourier)
	mux.HandleFunc("POST /v1/admin/couriers/{id}/token", f.reissue)
	mux.HandleFunc("POST /v1/quotes", f.quote)
	mux.HandleFunc("POST /v1/orders", f.createOrder)
	mux.HandleFunc("GET /v1/couriers/me/offers/stream", f.stream)
	mux.HandleFunc("POST /v1/offers/{id}/{verb}", f.answer)
	mux.HandleFunc("POST /v1/courier/orders/{id}/{action}", f.courierAction)
	mux.HandleFunc("POST /v1/locations", f.locations)
	mux.HandleFunc("GET /route/v1/bicycle/{coords}", f.route)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.close)
	return f
}

func (f *fakeTasuki) close() {
	f.mu.Lock()
	for id, ch := range f.streams {
		close(ch)
		delete(f.streams, id)
	}
	f.mu.Unlock()
	f.srv.Close()
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeTasuki) jwt(courierID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	claims := fmt.Sprintf(`{"sub":%q,"role":"courier","exp":%d}`, courierID, time.Now().Add(f.tokenTTL).Unix())
	token := header + "." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
	f.courierOf[token] = courierID
	return token
}

func (f *fakeTasuki) courierFor(r *http.Request) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.courierOf[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	return id, ok
}

func (f *fakeTasuki) createMerchant(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer admin" {
		reply(w, http.StatusUnauthorized, nil)
		return
	}
	var req struct {
		Name   string           `json:"name"`
		Stores []map[string]any `json:"stores"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.merchants++
	n := f.merchants
	f.mu.Unlock()
	for i, s := range req.Stores {
		s["store_id"] = fmt.Sprintf("str_%d_%d", n, i)
	}
	reply(w, http.StatusCreated, map[string]any{"merchant_id": fmt.Sprintf("mer_%d", n), "api_key": fmt.Sprintf("key_%d", n), "stores": req.Stores})
}

func (f *fakeTasuki) createCourier(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.couriers++
	id := fmt.Sprintf("cou_%d", f.couriers)
	reply(w, http.StatusCreated, map[string]string{"courier_id": id, "token": f.jwt(id)})
}

func (f *fakeTasuki) reissue(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reissued++
	f.tokenTTL = 24 * time.Hour // re-issued tokens are long-lived
	id := r.PathValue("id")
	reply(w, http.StatusOK, map[string]string{"courier_id": id, "token": f.jwt(id)})
}

func (f *fakeTasuki) quote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dropoff geo.Point `json:"dropoff"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	id := fmt.Sprintf("quo_%d", len(f.quotes)+1)
	f.quotes[id] = req.Dropoff
	f.mu.Unlock()
	reply(w, http.StatusCreated, map[string]any{"quote_id": id, "fee_yen": 660, "eta_seconds": 1800})
}

func (f *fakeTasuki) createOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QuoteID string `json:"quote_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	key := r.Header.Get("Idempotency-Key")
	f.mu.Lock()
	if id, seen := f.idempotency[key]; seen {
		f.replays++
		f.mu.Unlock()
		w.Header().Set("Idempotent-Replayed", "true")
		reply(w, http.StatusCreated, map[string]string{"order_id": id, "status": "confirmed"})
		return
	}
	f.orders++
	id := fmt.Sprintf("ord_%d", f.orders)
	f.idempotency[key] = id
	f.dropoffs[id] = f.quotes[req.QuoteID]
	f.mu.Unlock()
	reply(w, http.StatusCreated, map[string]string{"order_id": id, "status": "confirmed"})
	f.offer(id, "")
}

// offer sends the order to the next courier with an open stream (other than skip).
func (f *fakeTasuki) offer(orderID, skip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range len(f.order) {
		courierID := f.order[f.next%len(f.order)]
		f.next++
		ch, open := f.streams[courierID]
		if !open || courierID == skip {
			continue
		}
		f.offerSeq++
		offerID := fmt.Sprintf("off_%d", f.offerSeq)
		f.offers[offerID] = orderID
		d := f.dropoffs[orderID]
		event := fmt.Sprintf(`{"offer_id":%q,"order_id":%q,"store":{"id":"str_1","lat":35.664,"lng":139.698},"dropoff":{"lat":%f,"lng":%f},"expires_at":%q}`,
			offerID, orderID, d.Lat, d.Lng, time.Now().Add(30*time.Second).UTC().Format(time.RFC3339))
		select {
		case ch <- "event: offer\ndata: " + event + "\n\n":
		default: // that courier's stream is backed up; the order waits
		}
		return
	}
}

func (f *fakeTasuki) stream(w http.ResponseWriter, r *http.Request) {
	courierID, ok := f.courierFor(r)
	if !ok {
		reply(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "invalid_token"}})
		return
	}
	ch := make(chan string, 16)
	f.mu.Lock()
	f.streams[courierID] = ch
	f.order = append(f.order, courierID)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		if f.streams[courierID] == ch {
			delete(f.streams, courierID)
		}
		f.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	_, _ = io.WriteString(w, ": ping\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-ch:
			if !open {
				return
			}
			_, _ = io.WriteString(w, event)
			flusher.Flush()
		}
	}
}

func (f *fakeTasuki) answer(w http.ResponseWriter, r *http.Request) {
	courierID, _ := f.courierFor(r)
	f.mu.Lock()
	orderID := f.offers[r.PathValue("id")]
	f.mu.Unlock()
	verb := r.PathValue("verb")
	reply(w, http.StatusOK, map[string]string{"offer_id": r.PathValue("id"), "order_id": orderID, "status": verb + "ed"})
	if verb == "decline" {
		f.offer(orderID, courierID)
	}
}

func (f *fakeTasuki) courierAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.PathValue("action") == "deliver" {
		var proof geo.Point
		_ = json.NewDecoder(r.Body).Decode(&proof)
		f.mu.Lock()
		far := geo.Distance(proof, f.dropoffs[id]) > 500
		if far {
			f.badProofs++
		} else {
			f.delivered++
		}
		f.mu.Unlock()
		if far {
			reply(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]string{"code": "proof_too_far"}})
			return
		}
	}
	reply(w, http.StatusOK, map[string]string{"order_id": id, "status": r.PathValue("action")})
}

func (f *fakeTasuki) locations(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Points []json.RawMessage `json:"points"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.points += len(req.Points)
	f.mu.Unlock()
	reply(w, http.StatusAccepted, map[string]int{"accepted": len(req.Points), "dropped": 0})
}

// route answers with a polyline for the straight line between the two points.
func (f *fakeTasuki) route(w http.ResponseWriter, r *http.Request) {
	var pts []geo.Point
	for _, pair := range strings.Split(r.PathValue("coords"), ";") {
		lng, lat, _ := strings.Cut(pair, ",")
		x, _ := strconv.ParseFloat(lng, 64)
		y, _ := strconv.ParseFloat(lat, 64)
		pts = append(pts, geo.Point{Lat: y, Lng: x})
	}
	f.mu.Lock()
	f.routes++
	f.mu.Unlock()
	reply(w, http.StatusOK, map[string]any{"code": "Ok", "routes": []map[string]any{{"geometry": encodePolyline(pts)}}})
}

func encodePolyline(points []geo.Point) string {
	var out []byte
	var prevLat, prevLng int
	for _, p := range points {
		lat, lng := int(p.Lat*1e5+0.5), int(p.Lng*1e5+0.5)
		for _, v := range []int{lat - prevLat, lng - prevLng} {
			u := v << 1
			if v < 0 {
				u = ^u
			}
			for u >= 0x20 {
				out = append(out, byte((0x20|(u&0x1f))+63))
				u >>= 5
			}
			out = append(out, byte(u+63))
		}
		prevLat, prevLng = lat, lng
	}
	return string(out)
}

func (f *fakeTasuki) counts() (merchants, couriers, orders, delivered, points, reissued, routes, badProofs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.merchants, f.couriers, f.orders, f.delivered, f.points, f.reissued, f.routes, f.badProofs
}
