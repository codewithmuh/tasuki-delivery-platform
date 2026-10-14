package scenario

import (
	_ "embed" // for the go:embed directive below
	"encoding/json"
	"fmt"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
)

// wardsJSON is a copy of contracts/data/wards.json, embedded so the binary needs no data files.
// A test fails if the copy drifts from the contract.
//
//go:embed wards.json
var wardsJSON []byte

// Ward is one of Tokyo's 23 special wards. The centre is approximate and for synthetic data only.
type Ward struct {
	ID     string  `json:"id"`
	NameEN string  `json:"name_en"`
	NameJA string  `json:"name_ja"`
	Lat    float64 `json:"lat"`
	Lng    float64 `json:"lng"`
}

// Centre returns the ward's approximate centre.
func (w Ward) Centre() geo.Point { return geo.Point{Lat: w.Lat, Lng: w.Lng} }

type city struct {
	BBox  geo.BBox `json:"bbox"`
	Wards []Ward   `json:"wards"`
}

// tokyo is parsed once at start-up. The data is compiled in, so a parse error is a build
// mistake, not a runtime condition, and panicking is the honest response.
var tokyo = mustParseCity(wardsJSON)

func mustParseCity(data []byte) city {
	c, err := parseCity(data)
	if err != nil {
		panic(err)
	}
	return c
}

func parseCity(data []byte) (city, error) {
	var c city
	if err := json.Unmarshal(data, &c); err != nil {
		return city{}, fmt.Errorf("parse wards.json: %w", err)
	}
	if len(c.Wards) == 0 {
		return city{}, fmt.Errorf("parse wards.json: no wards")
	}
	return c, nil
}

// LookupWard returns the ward with the given ID (for example "shibuya").
func LookupWard(id string) (Ward, bool) {
	for _, w := range tokyo.Wards {
		if w.ID == id {
			return w, true
		}
	}
	return Ward{}, false
}

// AllWardIDs returns the IDs of all 23 wards in the order of wards.json.
func AllWardIDs() []string {
	ids := make([]string, len(tokyo.Wards))
	for i, w := range tokyo.Wards {
		ids[i] = w.ID
	}
	return ids
}

// CityBBox returns the bounding box around the 23 wards. Every drop-off must be inside it.
func CityBBox() geo.BBox { return tokyo.BBox }
