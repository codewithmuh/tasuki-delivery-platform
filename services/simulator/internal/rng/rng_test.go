package rng

import "testing"

func TestSameSeedAndNameRepeat(t *testing.T) {
	a, b := New(42, "courier/1"), New(42, "courier/1")
	for i := range 100 {
		if x, y := a.Uint64(), b.Uint64(); x != y {
			t.Fatalf("draw %d differs: %d vs %d", i, x, y)
		}
	}
}

func TestDifferentNamesOrSeedsDiffer(t *testing.T) {
	base := New(42, "courier/1").Uint64()
	if New(42, "courier/2").Uint64() == base {
		t.Error("different agent names gave the same first draw")
	}
	if New(43, "courier/1").Uint64() == base {
		t.Error("different seeds gave the same first draw")
	}
}
