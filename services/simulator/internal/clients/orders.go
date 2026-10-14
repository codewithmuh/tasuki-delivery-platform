package clients

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
)

// Orders calls the orders service: admin seeding, merchant quotes and orders, and courier
// pickup and delivery.
type Orders struct {
	endpoint
	adminToken string
	retry      Retry
}

// NewOrders returns a client for the orders service at baseURL (for example
// http://127.0.0.1:8000). adminToken authenticates the /v1/admin calls used for seeding.
func NewOrders(baseURL, adminToken string, httpClient *http.Client, observer Observer) *Orders {
	return &Orders{
		endpoint:   newEndpoint(baseURL, httpClient, 5*time.Second, observer),
		adminToken: adminToken,
		// Four attempts over roughly 10 s rides out a pod restart or a brief overload;
		// a database failover (1–2 min) still shows up as failed orders, as it should.
		retry: Retry{Attempts: 4, Base: 500 * time.Millisecond, Max: 5 * time.Second},
	}
}

// NewStore is a store to create with a merchant.
type NewStore struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
	Ward string  `json:"ward"`
}

// Store is a store as the orders service created it.
type Store struct {
	ID   string  `json:"store_id"`
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
	Ward string  `json:"ward"`
}

// Merchant is a newly created merchant with its API key. The key is shown only once.
type Merchant struct {
	ID     string  `json:"merchant_id"`
	APIKey string  `json:"api_key"`
	Stores []Store `json:"stores"`
}

// CreateMerchant creates a merchant (a tenant) with its stores.
func (o *Orders) CreateMerchant(ctx context.Context, name string, stores []NewStore) (Merchant, error) {
	var m Merchant
	err := o.withRetry(ctx, "admin_merchant", o.retry, func() error {
		_, err := o.do(ctx, request{
			call: "admin_merchant", method: http.MethodPost, path: "/v1/admin/merchants", token: o.adminToken,
			body: map[string]any{"name": name, "stores": stores},
		}, &m)
		return err
	})
	return m, err
}

// Courier is a courier's ID and the JWT the courier app uses for every call.
type Courier struct {
	ID    string `json:"courier_id"`
	Token string `json:"token"`
}

// CreateCourier creates a courier riding a "bicycle" or a "scooter".
func (o *Orders) CreateCourier(ctx context.Context, name, vehicle string) (Courier, error) {
	var c Courier
	err := o.withRetry(ctx, "admin_courier", o.retry, func() error {
		_, err := o.do(ctx, request{
			call: "admin_courier", method: http.MethodPost, path: "/v1/admin/couriers", token: o.adminToken,
			body: map[string]string{"name": name, "vehicle": vehicle},
		}, &c)
		return err
	})
	return c, err
}

// ReissueCourierToken issues a fresh JWT for an existing courier, before the old one expires.
func (o *Orders) ReissueCourierToken(ctx context.Context, courierID string) (Courier, error) {
	var c Courier
	err := o.withRetry(ctx, "admin_courier_token", o.retry, func() error {
		_, err := o.do(ctx, request{
			call: "admin_courier_token", method: http.MethodPost, token: o.adminToken,
			path: "/v1/admin/couriers/" + url.PathEscape(courierID) + "/token",
		}, &c)
		return err
	})
	return c, err
}

// Dropoff is where an order goes. The customer details are synthetic.
type Dropoff struct {
	Lat           float64 `json:"lat"`
	Lng           float64 `json:"lng"`
	Ward          string  `json:"ward"`
	Address       string  `json:"address"`
	CustomerName  string  `json:"customer_name"`
	CustomerPhone string  `json:"customer_phone"`
}

// Quote is a price and an ETA for a delivery, valid until ExpiresAt.
type Quote struct {
	ID            string    `json:"quote_id"`
	FeeYen        int64     `json:"fee_yen"`
	FeeExclTaxYen int64     `json:"fee_excl_tax_yen"`
	TaxYen        int64     `json:"tax_yen"`
	ETASeconds    int       `json:"eta_seconds"`
	PromisedAt    time.Time `json:"promised_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	ETASource     string    `json:"eta_source"` // "dispatch" or "fallback"
}

// Quote asks for a price and ETA for a delivery from a store. A retried quote request may
// create a second quote; that is harmless, because quotes expire unused after 2 minutes.
func (o *Orders) Quote(ctx context.Context, apiKey, storeID string, d Dropoff) (Quote, error) {
	var q Quote
	err := o.withRetry(ctx, "quote", o.retry, func() error {
		_, err := o.do(ctx, request{
			call: "quote", method: http.MethodPost, path: "/v1/quotes", token: apiKey,
			body: map[string]any{"store_id": storeID, "dropoff": d},
		}, &q)
		return err
	})
	return q, err
}

// Order is a confirmed order.
type Order struct {
	ID          string    `json:"order_id"`
	Status      string    `json:"status"`
	PromisedAt  time.Time `json:"promised_at"`
	FeeYen      int64     `json:"fee_yen"`
	TrackingURL string    `json:"tracking_url"`
	CreatedAt   time.Time `json:"created_at"`
	// Replayed is true when the answer came from the server's idempotency store: an earlier
	// attempt had already created the order.
	Replayed bool `json:"-"`
}

// CreateOrder turns a quote into an order. Every attempt sends the same idempotencyKey, so if
// an attempt times out after the server committed the order, the retry gets the original
// order back instead of creating a second one.
func (o *Orders) CreateOrder(ctx context.Context, apiKey, quoteID, idempotencyKey string) (Order, error) {
	var order Order
	err := o.withRetry(ctx, "order", o.retry, func() error {
		header, err := o.do(ctx, request{
			call: "order", method: http.MethodPost, path: "/v1/orders", token: apiKey,
			header: map[string]string{"Idempotency-Key": idempotencyKey},
			body:   map[string]string{"quote_id": quoteID},
		}, &order)
		if err == nil {
			order.Replayed = header.Get("Idempotent-Replayed") == "true"
		}
		return err
	})
	return order, err
}

// Pickup tells the orders service the courier has collected the order. It is not retried
// here: the courier tries again on its next tick.
func (o *Orders) Pickup(ctx context.Context, token, orderID string) error {
	_, err := o.do(ctx, request{
		call: "pickup", method: http.MethodPost, token: token,
		path: "/v1/courier/orders/" + url.PathEscape(orderID) + "/pickup",
	}, nil)
	return err
}

// Deliver reports the order delivered, with the courier's position as proof. The orders
// service rejects proof more than 500 m from the drop-off.
func (o *Orders) Deliver(ctx context.Context, token, orderID string, at geo.Point) error {
	_, err := o.do(ctx, request{
		call: "deliver", method: http.MethodPost, token: token,
		path: "/v1/courier/orders/" + url.PathEscape(orderID) + "/deliver",
		body: at,
	}, nil)
	return err
}

// WaitReady polls the orders service's /readyz until it answers 200 or ctx ends. Seeding
// waits for it, so `docker compose up` can start everything at once.
func (o *Orders) WaitReady(ctx context.Context, every time.Duration) error {
	for {
		_, err := o.do(ctx, request{call: "orders_readyz", method: http.MethodGet, path: "/readyz"}, nil)
		if err == nil {
			return nil
		}
		if sleepErr := sleep(ctx, every); sleepErr != nil {
			return err
		}
	}
}
