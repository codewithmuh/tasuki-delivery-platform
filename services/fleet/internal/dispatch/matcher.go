package dispatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// minCandidates is how many free couriers the search wants before it stops widening its radius:
// ranking by road time needs a choice, because the nearest courier in a straight line can be the
// farthest by road (across a river or a rail line).
const minCandidates = 3

// MatchConfig tunes greedy matching (design §7.3, P1).
type MatchConfig struct {
	Interval         time.Duration // MATCH_INTERVAL: how often due orders are looked at
	OfferTTL         time.Duration // OFFER_TTL
	SearchRadiusM    float64       // SEARCH_RADIUS_M: first search radius around the store
	SearchMaxRadiusM float64       // SEARCH_MAX_RADIUS_M: the radius doubles up to this
	MaxCandidates    int           // SEARCH_MAX_CANDIDATES: couriers ranked by road time per attempt
	MaxAttempts      int           // MAX_OFFER_ATTEMPTS: offers before NoCourierAvailable is reported
	NoCourierAfter   time.Duration // NO_COURIER_AFTER: report when no offer could be made this long after confirmation
	ReportInterval   time.Duration // NO_COURIER_REPORT_INTERVAL: repeat NoCourierAvailable at most this often
	RetryInterval    time.Duration // RETRY_INTERVAL: wait between attempts that found nobody
	RoutingTimeout   time.Duration // ROUTING_TIMEOUT: OSRM budget per attempt
}

// gate says whether matching may run (false while Redis is being recovered).
type gate interface{ Dispatching() bool }

// Matcher offers each pending order to the nearest free courier by road time (greedy, P1). It
// works on the orders of the partitions this replica owns; the atomic scripts keep it correct
// even when two replicas briefly own the same partition during a rebalance.
type Matcher struct {
	cfg     MatchConfig
	store   *Store
	router  *Router
	outbox  *Outbox
	notify  *Notifier
	owner   *Ownership
	gate    gate
	metrics *metrics
	logger  *slog.Logger
	now     func() time.Time
	wake    chan struct{}
}

// Wake asks for a matching pass now, for example after a new order arrived. It never blocks.
func (m *Matcher) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run matches every Interval, and when woken, until ctx is cancelled.
func (m *Matcher) Run(ctx context.Context) error {
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-m.wake:
		}
		m.Tick(ctx)
	}
}

// Tick makes one pass over the owned pending orders, oldest first: the oldest order has waited
// longest, so a declined or expired order comes back ahead of newer ones (design §7.3).
func (m *Matcher) Tick(ctx context.Context) {
	if !m.gate.Dispatching() {
		return
	}
	orders, err := m.store.PendingOrders(ctx, m.owner.Partitions())
	if err != nil {
		m.logger.Warn("could not read pending orders", "err", err)
		return
	}
	sort.Slice(orders, func(i, j int) bool {
		if !orders[i].ConfirmedAt.Equal(orders[j].ConfirmedAt) {
			return orders[i].ConfirmedAt.Before(orders[j].ConfirmedAt)
		}
		return orders[i].ID < orders[j].ID
	})
	byWard := map[string]int{}
	for _, o := range orders {
		byWard[o.Ward]++
	}
	m.metrics.setPending(byWard)

	for i := range orders {
		if ctx.Err() != nil {
			return
		}
		o, now := &orders[i], m.now()
		switch {
		case o.Status == statusOffered && !now.Before(o.OfferExpiresAt):
			if m.expire(ctx, o, now) {
				m.attempt(ctx, o, now) // offer it to the next courier straight away
			}
		case o.Status == statusPending && !now.Before(o.NextAt):
			m.attempt(ctx, o, now)
		}
	}
}

// attempt tries to make one offer for a pending order.
func (m *Matcher) attempt(ctx context.Context, o *Order, now time.Time) {
	exhausted := o.Attempts >= m.cfg.MaxAttempts
	if exhausted {
		// Report before trying again, so orders hears about it even while couriers keep declining.
		m.noOffer(ctx, o, now, true)
	}
	cands, err := m.candidates(ctx, o)
	if err != nil {
		m.logger.Warn("courier search failed", "order_id", o.ID, "err", err)
		return
	}
	if len(cands) > 0 {
		ranked, dropoffS := m.rank(ctx, o, cands)
		for _, c := range ranked {
			res, err := m.offer(ctx, o, c, dropoffS, now)
			if err != nil {
				m.logger.Warn("could not create an offer", "order_id", o.ID, "err", err)
				return
			}
			if res == resultOffered || res == resultOrderMoved {
				return
			}
			// courier_busy / courier_reserved: another order (maybe on another replica) got them first.
		}
	}
	m.noOffer(ctx, o, now, exhausted || now.Sub(o.ConfirmedAt) >= m.cfg.NoCourierAfter)
}

// candidates returns free couriers near the store, nearest first, widening the radius until
// there are enough to choose from or the maximum radius is reached.
func (m *Matcher) candidates(ctx context.Context, o *Order) ([]Nearby, error) {
	tried, err := m.store.Tried(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	radius := m.cfg.SearchRadiusM
	for {
		// Ask for more than needed: some of the nearest may be engaged or have said no already.
		found, err := m.store.Nearby(ctx, o.Store, radius, 3*m.cfg.MaxCandidates)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(found))
		for i, f := range found {
			ids[i] = f.CourierID
		}
		engaged, err := m.store.Engaged(ctx, ids)
		if err != nil {
			return nil, err
		}
		free := make([]Nearby, 0, len(found))
		for _, f := range found {
			if !engaged[f.CourierID] && !tried[f.CourierID] && len(free) < m.cfg.MaxCandidates {
				free = append(free, f)
			}
		}
		if len(free) >= minCandidates || radius >= m.cfg.SearchMaxRadiusM {
			return free, nil
		}
		radius = min(2*radius, m.cfg.SearchMaxRadiusM)
	}
}

type rankedCourier struct {
	Nearby
	PickupS int // road time from the courier to the store
}

// rank orders candidates by road time to the store with one OSRM table call, which also gives
// the store-to-drop-off time.
func (m *Matcher) rank(ctx context.Context, o *Order, cands []Nearby) ([]rankedCourier, int) {
	sources := make([]Point, 0, len(cands)+1)
	for _, c := range cands {
		sources = append(sources, c.Point)
	}
	sources = append(sources, o.Store)
	d, _ := m.router.Durations(ctx, sources, []Point{o.Store, o.Dropoff}, m.cfg.RoutingTimeout)
	ranked := make([]rankedCourier, len(cands))
	for i, c := range cands {
		ranked[i] = rankedCourier{Nearby: c, PickupS: d[i][0]}
	}
	// Stable: on equal road times the straight-line order (nearest first) decides.
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].PickupS < ranked[j].PickupS })
	return ranked, d[len(cands)][1]
}

// offerData is the SSE "offer" event (contracts/README.md, server-sent event streams).
type offerData struct {
	OfferID     string     `json:"offer_id"`
	OrderID     string     `json:"order_id"`
	Store       offerStore `json:"store"`
	Dropoff     Point      `json:"dropoff"`
	ReadyAt     *time.Time `json:"ready_at"` // null when the store gave no time
	ExpiresAt   time.Time  `json:"expires_at"`
	EtaPickupS  int        `json:"eta_pickup_s"`
	EtaDropoffS int        `json:"eta_dropoff_s"`
}

type offerStore struct {
	ID  string  `json:"id"`
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// offer creates one offer and, if the courier could be reserved, sends it.
func (m *Matcher) offer(ctx context.Context, o *Order, c rankedCourier, dropoffS int, now time.Time) (string, error) {
	offerID := events.NewID(events.PrefixOffer)
	expires := now.Add(m.cfg.OfferTTL).UTC()
	payload, err := json.Marshal(offerData{
		OfferID: offerID, OrderID: o.ID, Store: offerStore{ID: o.StoreID, Lat: o.Store.Lat, Lng: o.Store.Lng},
		Dropoff: o.Dropoff, ReadyAt: o.ReadyAt, ExpiresAt: expires, EtaPickupS: c.PickupS, EtaDropoffS: dropoffS,
	})
	if err != nil {
		return "", err
	}
	sent := newEvent(events.OfferSent, o.ID, o.Ward, now)
	sent.CourierID, sent.OfferID, sent.OfferExpiresAt = &c.CourierID, &offerID, &expires
	sent.EtaPickupS, sent.EtaDropoffS = &c.PickupS, &dropoffS
	evs, err := encode(sent)
	if err != nil {
		return "", err
	}
	res, err := m.store.CreateOffer(ctx, now, newOffer{
		Order: *o, OfferID: offerID, CourierID: c.CourierID, ExpiresAt: expires,
		EtaPickupS: c.PickupS, EtaDropoffS: dropoffS, Payload: payload, Events: evs,
	})
	if err != nil || res != resultOffered {
		return res, err
	}
	m.notify.Offer(ctx, c.CourierID, offerID, payload)
	m.outbox.Send(ctx, evs)
	m.metrics.offers.WithLabelValues("sent").Inc()
	m.logger.Debug("offer sent", "order_id", o.ID, "offer_id", offerID, "courier_id", c.CourierID,
		"ward", o.Ward, "eta_pickup_s", c.PickupS, "attempt", o.Attempts+1)
	return res, nil
}

// expire ends an offer nobody answered in time and reports whether it did; the order is then
// pending again and may be offered to the next courier at once.
func (m *Matcher) expire(ctx context.Context, o *Order, now time.Time) bool {
	ev := newEvent(events.OfferExpired, o.ID, o.Ward, now)
	ev.CourierID, ev.OfferID = &o.CourierID, &o.OfferID
	evs, err := encode(ev)
	if err != nil {
		return false
	}
	ok, err := m.store.ExpireOffer(ctx, *o, now, now, evs)
	if err != nil {
		m.logger.Warn("could not expire an offer", "order_id", o.ID, "offer_id", o.OfferID, "err", err)
		return false
	}
	if !ok {
		return false // answered just in time
	}
	m.outbox.Send(ctx, evs)
	m.notify.Cancelled(ctx, o.CourierID, o.OfferID, reasonExpired)
	m.metrics.offers.WithLabelValues("expired").Inc()
	m.logger.Info("offer expired", "order_id", o.ID, "offer_id", o.OfferID, "courier_id", o.CourierID, "ward", o.Ward)
	o.Status, o.NextAt, o.OfferID, o.CourierID = statusPending, now, "", ""
	return true
}

// noOffer schedules the next attempt and, when report is set, emits NoCourierAvailable (at most
// once per NO_COURIER_REPORT_INTERVAL). orders keeps the order waiting until it is 15 minutes
// old, then cancels it, so the report must repeat while the order waits.
func (m *Matcher) noOffer(ctx context.Context, o *Order, now time.Time, report bool) {
	evs, err := encode(newEvent(events.NoCourierAvailable, o.ID, o.Ward, now))
	if err != nil {
		return
	}
	reported, first, err := m.store.NoOffer(ctx, *o, now, now.Add(m.cfg.RetryInterval), report, m.cfg.ReportInterval, evs)
	if err != nil {
		m.logger.Warn("could not record a failed attempt", "order_id", o.ID, "err", err)
		return
	}
	if !reported {
		return
	}
	m.outbox.Send(ctx, evs)
	m.logger.Info("NoCourierAvailable", "order_id", o.ID, "ward", o.Ward, "attempts", o.Attempts,
		"waited_s", int(now.Sub(o.ConfirmedAt).Seconds()))
	if first {
		m.metrics.assignment.WithLabelValues(outcomeNoCourier).Observe(now.Sub(o.ConfirmedAt).Seconds())
	}
}
