package geo

import (
	"errors"
	"fmt"
	"math"
)

// DecodePolyline decodes a Google encoded polyline, the compact text format OSRM uses for route
// geometry (geometries=polyline is precision 5, polyline6 is precision 6).
//
// The format stores each coordinate as the difference from the previous one, multiplied by
// 10^precision and rounded to an integer, then written as printable ASCII. See
// https://developers.google.com/maps/documentation/utilities/polylinealgorithm.
func DecodePolyline(s string, precision int) ([]Point, error) {
	factor := math.Pow10(precision)
	var points []Point
	var lat, lng int // running totals: each value in the string is a delta
	for i := 0; i < len(s); {
		dLat, n, err := decodeValue(s[i:])
		if err != nil {
			return nil, fmt.Errorf("latitude at offset %d: %w", i, err)
		}
		i += n
		dLng, n, err := decodeValue(s[i:])
		if err != nil {
			return nil, fmt.Errorf("longitude at offset %d: %w", i, err)
		}
		i += n
		lat += dLat
		lng += dLng
		points = append(points, Point{Lat: float64(lat) / factor, Lng: float64(lng) / factor})
	}
	return points, nil
}

// decodeValue reads one signed number from the start of s and returns it with the number of
// bytes it used.
//
// A number is split into 5-bit chunks, least significant first. Each chunk is stored as one
// character: the chunk, plus 0x20 if more chunks follow, plus 63 so it is printable. After
// joining the chunks, the lowest bit is the sign: if it is set, the value was negative and the
// remaining bits are inverted.
func decodeValue(s string) (value, used int, err error) {
	var result, shift int
	for i := range len(s) {
		chunk := int(s[i]) - 63
		if chunk < 0 || chunk > 63 {
			return 0, 0, fmt.Errorf("invalid character %q", s[i])
		}
		result |= (chunk & 0x1f) << shift
		shift += 5
		if chunk < 0x20 { // no continuation bit: this was the last chunk
			if result&1 == 1 {
				return ^(result >> 1), i + 1, nil
			}
			return result >> 1, i + 1, nil
		}
		if shift > 60 {
			return 0, 0, errors.New("value too long")
		}
	}
	return 0, 0, errors.New("truncated value")
}
