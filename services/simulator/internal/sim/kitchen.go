package sim

import (
	"sync"
	"time"
)

// kitchen is the simulated stores' order book: when each order's food will be ready. The
// customer agents write to it when an order is created; couriers read it when they accept
// an offer. It implements customer.Kitchen and courier.Kitchen.
type kitchen struct {
	mu    sync.Mutex
	ready map[string]time.Time
}

// keepFor is how long an entry outlives its ready time. An order nobody picks up within an
// hour of being ready is no longer interesting.
const keepFor = time.Hour

func newKitchen() *kitchen { return &kitchen{ready: map[string]time.Time{}} }

// Prepare records when an order will be ready.
func (k *kitchen) Prepare(orderID string, readyAt time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ready[orderID] = readyAt
	if len(k.ready) > 10_000 { // keep memory flat on long runs
		cutoff := time.Now().Add(-keepFor)
		for id, at := range k.ready {
			if at.Before(cutoff) {
				delete(k.ready, id)
			}
		}
	}
}

// ReadyAt returns when an order will be ready, if the simulator placed it.
func (k *kitchen) ReadyAt(orderID string) (time.Time, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	at, ok := k.ready[orderID]
	return at, ok
}
