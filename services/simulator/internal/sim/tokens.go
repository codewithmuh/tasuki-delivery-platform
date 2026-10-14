package sim

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
)

// tokens holds every courier's current JWT. Couriers read through it on every call, so a
// re-issued token takes effect straight away.
type tokens struct {
	mu   sync.RWMutex
	byID map[string]string
}

func newTokens(couriers []SeededCourier) *tokens {
	t := &tokens{byID: make(map[string]string, len(couriers))}
	for _, c := range couriers {
		t.byID[c.ID] = c.Token
	}
	return t
}

func (t *tokens) get(courierID string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byID[courierID]
}

func (t *tokens) set(courierID, token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byID[courierID] = token
}

// source returns the function a courier calls for its current token.
func (t *tokens) source(courierID string) func() string {
	return func() string { return t.get(courierID) }
}

// tokenExpiry reads the exp claim of a JWT without checking its signature. The simulator does
// not need to trust the token, only to know when to ask for a new one.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// Reissuer is the orders admin call that issues a fresh courier token.
type Reissuer interface {
	ReissueCourierToken(ctx context.Context, courierID string) (clients.Courier, error)
}

// refreshTokens re-issues every token that expires within `ahead`, updating st and the token
// store. It reports whether anything changed, so the caller can save the state file.
func refreshTokens(ctx context.Context, st *State, store *tokens, orders Reissuer, ahead time.Duration, logger *slog.Logger) bool {
	changed := false
	for i, c := range st.Couriers {
		exp, ok := tokenExpiry(store.get(c.ID))
		if !ok || time.Until(exp) > ahead {
			continue
		}
		fresh, err := orders.ReissueCourierToken(ctx, c.ID)
		if err != nil {
			logger.Warn("could not re-issue a courier token", "courier_id", c.ID, "expires", exp, "err", err)
			continue
		}
		store.set(c.ID, fresh.Token)
		st.Couriers[i].Token = fresh.Token
		changed = true
	}
	return changed
}
