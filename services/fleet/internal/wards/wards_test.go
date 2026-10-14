package wards

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The embedded copy must equal the contract file. If this fails, run `go generate ./...`.
func TestEmbeddedCopyMatchesContract(t *testing.T) {
	contract, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", "data", "wards.json"))
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	if !bytes.Equal(contract, raw) {
		t.Fatal("internal/wards/wards.json differs from contracts/data/wards.json; run `go generate ./...`")
	}
}

func TestEveryWardCentreIsNearestToItself(t *testing.T) {
	all := All()
	if len(all) != 23 {
		t.Fatalf("got %d wards, want Tokyo's 23", len(all))
	}
	for _, w := range all {
		if got := Nearest(w.Lat, w.Lng); got != w.ID {
			t.Errorf("Nearest(centre of %s) = %s", w.ID, got)
		}
		if !Known(w.ID) {
			t.Errorf("Known(%s) = false", w.ID)
		}
	}
}

func TestNearest(t *testing.T) {
	if got := Nearest(35.6595, 139.7005); got != "shibuya" { // Shibuya crossing
		t.Errorf("Shibuya crossing is in %q", got)
	}
	if got := Nearest(35.0116, 135.7681); got != "" { // Kyoto
		t.Errorf("Kyoto should be outside the 23 wards, got %q", got)
	}
	if Known("yokohama") {
		t.Error("yokohama is not a ward")
	}
}

func TestAllReturnsACopy(t *testing.T) {
	a := All()
	a[0].ID = "changed"
	if All()[0].ID == "changed" {
		t.Fatal("All must not expose the package's slice")
	}
}

func TestMustParsePanicsOnBadData(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	mustParse([]byte(`{"wards": []}`))
}
