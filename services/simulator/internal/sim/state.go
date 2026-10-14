package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/scenario"
)

// State is what seeding created. The orders service shows a merchant's API key and a
// courier's token only once, so they are saved to the state file: a restart with the same
// scenario reuses the same merchants and couriers instead of creating new ones.
//
// The file holds credentials (for fictional tenants, but credentials all the same): it is
// written with mode 0600, is git-ignored, and is never logged.
type State struct {
	Fingerprint string           `json:"fingerprint"`
	CreatedAt   time.Time        `json:"created_at"`
	Merchants   []SeededMerchant `json:"merchants"`
	Couriers    []SeededCourier  `json:"couriers"`
}

// SeededMerchant is a merchant created by seeding.
type SeededMerchant struct {
	ID     string          `json:"merchant_id"`
	Name   string          `json:"name"`
	APIKey string          `json:"api_key"`
	Stores []clients.Store `json:"stores"`
}

// SeededCourier is a courier created by seeding. Index is its position in the scenario,
// which (with the seed) decides its vehicle, home and shift.
type SeededCourier struct {
	Index   int    `json:"index"`
	ID      string `json:"courier_id"`
	Token   string `json:"token"`
	Vehicle string `json:"vehicle"`
}

// fingerprint identifies what a state file was seeded for. If anything that seeding depends
// on changes (the seed, the wards, the counts, or which orders service), the file is stale.
func fingerprint(s *scenario.Scenario, ordersURL string) string {
	key, _ := json.Marshal(struct { // marshalling plain values cannot fail
		Name          string
		Seed          uint64
		Wards         []string
		RadiusM       float64
		Merchants     int
		StoresPerWard int
		Couriers      int
		ScooterShare  float64
		OrdersURL     string
	}{s.Name, s.Seed, s.Wards, s.WardRadiusM, s.Merchants, s.StoresPerWard, s.Couriers.Count, s.Couriers.ScooterShare, ordersURL})
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// loadState reads the state file. A missing file is not an error: it returns nil.
func loadState(path string) (*State, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's STATE_FILE setting
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	return &st, nil
}

// saveState writes the state file atomically (a temporary file, then a rename), so a crash
// mid-write never leaves a half-written file behind.
func saveState(path string, st *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}
