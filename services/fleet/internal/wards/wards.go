// Package wards knows Tokyo's 23 special wards, the dispatch zones (contracts/data/wards.json).
//
// The JSON file is embedded into the binary. go:embed cannot reach outside the module, so
// wards.json here is a copy of the contract file; `go generate` refreshes it and a test fails
// if the two ever differ.
package wards

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
)

//go:generate cp ../../../../contracts/data/wards.json wards.json

//go:embed wards.json
var raw []byte

// Ward is one special ward with an approximate centre (synthetic data, not survey data).
type Ward struct {
	ID     string  `json:"id"`
	NameEN string  `json:"name_en"`
	NameJA string  `json:"name_ja"`
	Lat    float64 `json:"lat"`
	Lng    float64 `json:"lng"`
}

type bbox struct {
	MinLat float64 `json:"min_lat"`
	MaxLat float64 `json:"max_lat"`
	MinLng float64 `json:"min_lng"`
	MaxLng float64 `json:"max_lng"`
}

type file struct {
	BBox  bbox   `json:"bbox"`
	Wards []Ward `json:"wards"`
}

var data = mustParse(raw)

func mustParse(b []byte) file {
	var f file
	if err := json.Unmarshal(b, &f); err != nil || len(f.Wards) == 0 {
		panic(fmt.Sprintf("wards.json is invalid: %v", err)) // embedded at build time; tests catch this
	}
	return f
}

// All returns the wards in file order.
func All() []Ward {
	return append([]Ward(nil), data.Wards...)
}

// Known reports whether id is a ward ID.
func Known(id string) bool {
	for _, w := range data.Wards {
		if w.ID == id {
			return true
		}
	}
	return false
}

// Nearest returns the ID of the ward whose centre is closest to the point, or "" when the point
// lies outside the 23 wards' bounding box. Nearest-centre is a P1 approximation of real ward
// boundaries (contracts/README.md); it is good enough to filter a live map.
func Nearest(lat, lng float64) string {
	b := data.BBox
	if lat < b.MinLat || lat > b.MaxLat || lng < b.MinLng || lng > b.MaxLng {
		return ""
	}
	// Equirectangular approximation: at city scale the error is far below the precision of
	// "ward centre" itself, and it avoids trigonometry per ward.
	kx := math.Cos(lat * math.Pi / 180)
	best, bestD := "", math.Inf(1)
	for _, w := range data.Wards {
		dx, dy := (lng-w.Lng)*kx, lat-w.Lat
		if d := dx*dx + dy*dy; d < bestD {
			best, bestD = w.ID, d
		}
	}
	return best
}
