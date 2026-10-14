package geo

import "testing"

func TestDecodePolyline(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		precision int
		want      []Point
	}{
		{
			// The worked example from Google's polyline algorithm documentation.
			name: "google example", in: "_p~iF~ps|U_ulLnnqC_mqNvxq`@", precision: 5,
			want: []Point{{38.5, -120.2}, {40.7, -120.95}, {43.252, -126.453}},
		},
		{
			// Tokyo coordinates: positive latitude and longitude above 100.
			name: "tokyo", in: encodeForTest([]Point{shibuya, shinjuku}, 5), precision: 5,
			want: []Point{shibuya, shinjuku},
		},
		{
			name: "precision 6", in: encodeForTest([]Point{{35.658034, 139.701636}}, 6), precision: 6,
			want: []Point{{35.658034, 139.701636}},
		},
		{name: "empty", in: "", precision: 5, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodePolyline(tc.in, tc.precision)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d points, want %d: %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if !near(got[i].Lat, tc.want[i].Lat, 1e-9) || !near(got[i].Lng, tc.want[i].Lng, 1e-9) {
					t.Errorf("point %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestDecodePolylineRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"truncated latitude":  "_p~i",      // continuation bit set on the last character
		"missing longitude":   "_p~iF",     // a latitude with no longitude after it
		"invalid character":   "_p~iF\x01", // below the printable range
		"absurdly long value": "~~~~~~~~~~~~~~",
	} {
		if _, err := DecodePolyline(in, 5); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// encodeForTest is the reverse of DecodePolyline, used only to build test inputs.
func encodeForTest(points []Point, precision int) string {
	factor := 1.0
	for range precision {
		factor *= 10
	}
	var out []byte
	var prevLat, prevLng int
	for _, p := range points {
		lat, lng := int(roundHalfAway(p.Lat*factor)), int(roundHalfAway(p.Lng*factor))
		out = appendValue(out, lat-prevLat)
		out = appendValue(out, lng-prevLng)
		prevLat, prevLng = lat, lng
	}
	return string(out)
}

func appendValue(out []byte, v int) []byte {
	u := v << 1
	if v < 0 {
		u = ^u
	}
	for u >= 0x20 {
		out = append(out, byte((0x20|(u&0x1f))+63))
		u >>= 5
	}
	return append(out, byte(u+63))
}

func roundHalfAway(x float64) float64 {
	if x < 0 {
		return float64(int(x - 0.5))
	}
	return float64(int(x + 0.5))
}
