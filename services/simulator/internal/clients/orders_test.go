package clients

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
)

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode request body: %v", err)
	}
	return body
}

func TestCreateMerchantSendsTheAdminTokenAndStores(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/admin/merchants" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer admin-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Header.Get("X-Request-Id") == "" {
			t.Error("missing X-Request-Id")
		}
		body := decodeBody(t, r)
		stores := body["stores"].([]any)
		if body["name"] != "Sim Kitchen 01" || len(stores) != 1 || stores[0].(map[string]any)["ward"] != "shibuya" {
			t.Errorf("body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"merchant_id": "mer_1", "api_key": "key_1",
			"stores": [{"store_id": "str_1", "name": "Shibuya #1", "lat": 35.66, "lng": 139.70, "ward": "shibuya"}]}`)
	}))
	defer srv.Close()

	o := NewOrders(srv.URL, "admin-secret", nil, nil)
	m, err := o.CreateMerchant(context.Background(), "Sim Kitchen 01",
		[]NewStore{{Name: "Shibuya #1", Lat: 35.66, Lng: 139.70, Ward: "shibuya"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "mer_1" || m.APIKey != "key_1" || len(m.Stores) != 1 || m.Stores[0].ID != "str_1" {
		t.Fatalf("merchant = %+v", m)
	}
}

func TestCreateCourierAndReissueToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/admin/couriers":
			if body := decodeBody(t, r); body["vehicle"] != "scooter" || body["name"] != "Sim Courier 0001" {
				t.Errorf("body = %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"courier_id": "cou_1", "token": "jwt-1"}`)
		case "/v1/admin/couriers/cou_1/token":
			_, _ = io.WriteString(w, `{"courier_id": "cou_1", "token": "jwt-2"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	o := NewOrders(srv.URL, "admin", nil, nil)
	c, err := o.CreateCourier(context.Background(), "Sim Courier 0001", "scooter")
	if err != nil || c.ID != "cou_1" || c.Token != "jwt-1" {
		t.Fatalf("CreateCourier = %+v, %v", c, err)
	}
	c, err = o.ReissueCourierToken(context.Background(), "cou_1")
	if err != nil || c.Token != "jwt-2" {
		t.Fatalf("ReissueCourierToken = %+v, %v", c, err)
	}
}

func TestQuoteSendsTheDropoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer merchant-key" {
			t.Errorf("Authorization = %q", got)
		}
		body := decodeBody(t, r)
		dropoff := body["dropoff"].(map[string]any)
		for _, field := range []string{"lat", "lng", "ward", "address", "customer_name", "customer_phone"} {
			if _, ok := dropoff[field]; !ok {
				t.Errorf("dropoff is missing %q", field)
			}
		}
		if body["store_id"] != "str_1" {
			t.Errorf("store_id = %v", body["store_id"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"quote_id": "quo_1", "fee_yen": 660, "fee_excl_tax_yen": 600, "tax_yen": 60,
			"eta_seconds": 1920, "promised_at": "2026-10-14T03:32:00Z", "expires_at": "2026-10-14T03:02:00Z", "eta_source": "dispatch"}`)
	}))
	defer srv.Close()

	q, err := NewOrders(srv.URL, "", nil, nil).Quote(context.Background(), "merchant-key", "str_1", Dropoff{
		Lat: 35.66, Lng: 139.70, Ward: "shibuya", Address: "1-2-3 Shibuya (simulated)",
		CustomerName: "Sim Customer", CustomerPhone: "090-0000-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.ID != "quo_1" || q.FeeYen != 660 || q.TaxYen != 60 || q.ETASeconds != 1920 || q.ETASource != "dispatch" ||
		!q.PromisedAt.Equal(time.Date(2026, 10, 14, 3, 32, 0, 0, time.UTC)) {
		t.Fatalf("quote = %+v", q)
	}
}

// The idempotency test: an order request fails with 503, then times out after the server has
// committed the order, then is retried. Every attempt must carry the same Idempotency-Key and
// body, and the final answer is the server's replay of the original order.
func TestCreateOrderRetriesWithTheSameIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		bodies = append(bodies, string(data))
		attempt := len(keys)
		mu.Unlock()

		switch attempt {
		case 1: // the database is failing over
			w.Header().Set("Retry-After", "0")
			writeError(w, http.StatusServiceUnavailable, "unavailable")
		case 2: // the order is committed, but the answer is too slow for the client
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
		default: // the retry is answered from the idempotency store
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"order_id": "ord_1", "status": "confirmed", "fee_yen": 660,
				"tracking_url": "https://tasuki.example/t/x", "promised_at": "2026-10-14T03:32:00Z", "created_at": "2026-10-14T03:00:00Z"}`)
		}
	}))
	defer srv.Close()

	obs := &recorder{}
	o := NewOrders(srv.URL, "", nil, obs)
	o.retry = fastRetry
	o.timeout = 50 * time.Millisecond

	order, err := o.CreateOrder(context.Background(), "key", "quo_1", "idem-123")
	if err != nil {
		t.Fatal(err)
	}
	if order.ID != "ord_1" || !order.Replayed {
		t.Fatalf("order = %+v, want ord_1 replayed", order)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 {
		t.Fatalf("server saw %d attempts, want 3", len(keys))
	}
	for i := range keys {
		if keys[i] != "idem-123" || bodies[i] != `{"quote_id":"quo_1"}` {
			t.Errorf("attempt %d: key %q body %s", i+1, keys[i], bodies[i])
		}
	}
	calls, retries := obs.snapshot()
	if len(retries) != 2 || calls[0] != "order 503" || calls[1] != "order timeout" || calls[2] != "order 201" {
		t.Errorf("observed calls %v, retries %v", calls, retries)
	}
}

func TestCreateOrderDoesNotRetryClientErrors(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		writeError(w, http.StatusConflict, "quote_already_used")
	}))
	defer srv.Close()

	o := NewOrders(srv.URL, "", nil, nil)
	o.retry = fastRetry
	_, err := o.CreateOrder(context.Background(), "key", "quo_1", "idem-1")
	if attempts != 1 || StatusOf(err) != http.StatusConflict || CodeOf(err) != "quote_already_used" {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestCreateOrderGivesUpAfterTheLastAttempt(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		writeError(w, http.StatusInternalServerError, "internal")
	}))
	defer srv.Close()

	o := NewOrders(srv.URL, "", nil, nil)
	o.retry = fastRetry
	_, err := o.CreateOrder(context.Background(), "key", "quo_1", "idem-1")
	if attempts != 4 || Reason(err) != "http_500" {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestRetriesStopWhenTheContextEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
	}))
	defer srv.Close()

	o := NewOrders(srv.URL, "", nil, nil)
	o.retry = Retry{Attempts: 10, Base: time.Hour, Max: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := o.CreateOrder(ctx, "key", "quo_1", "idem-1")
	if StatusOf(err) != http.StatusServiceUnavailable || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestPickupAndDeliver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer courier-jwt" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/v1/courier/orders/ord_1/pickup":
			data, _ := io.ReadAll(r.Body)
			if len(data) != 0 || r.Header.Get("Content-Type") != "" {
				t.Errorf("pickup should have no body, got %q (%s)", data, r.Header.Get("Content-Type"))
			}
			_, _ = io.WriteString(w, `{"order_id": "ord_1", "status": "picked_up"}`)
		case "/v1/courier/orders/ord_1/deliver":
			body := decodeBody(t, r)
			if body["lat"] != 35.66 || body["lng"] != 139.7 || len(body) != 2 {
				t.Errorf("deliver body = %v", body)
			}
			_, _ = io.WriteString(w, `{"order_id": "ord_1", "status": "delivered"}`)
		case "/v1/courier/orders/ord_2/deliver":
			writeError(w, http.StatusUnprocessableEntity, "proof_too_far")
		}
	}))
	defer srv.Close()

	o := NewOrders(srv.URL, "", nil, nil)
	if err := o.Pickup(context.Background(), "courier-jwt", "ord_1"); err != nil {
		t.Fatal(err)
	}
	if err := o.Deliver(context.Background(), "courier-jwt", "ord_1", geo.Point{Lat: 35.66, Lng: 139.7}); err != nil {
		t.Fatal(err)
	}
	err := o.Deliver(context.Background(), "courier-jwt", "ord_2", geo.Point{})
	if CodeOf(err) != "proof_too_far" {
		t.Fatalf("got %v", err)
	}
}

func TestWaitReady(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/readyz" || calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"status": "ready"}`)
	}))
	defer srv.Close()

	o := NewOrders(srv.URL, "", nil, nil)
	if err := o.WaitReady(context.Background(), time.Millisecond); err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewOrders("http://127.0.0.1:1", "", nil, nil).WaitReady(ctx, time.Millisecond); err == nil {
		t.Fatal("expected an error once the context is cancelled")
	}
}
