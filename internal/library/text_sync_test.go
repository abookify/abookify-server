package library

import "testing"

// interpFrac must linearly interpolate audio time between aligned-segment
// anchors and clamp outside their range — the basis-robust core of the #210
// paragraph-follow time mapping.
func TestInterpFrac(t *testing.T) {
	anchors := []fracAnchor{{0.0, 10}, {0.5, 20}, {1.0, 40}}
	cases := []struct {
		frac, want float64
	}{
		{-0.2, 10},  // below first → first sec
		{0.0, 10},   // at first
		{0.25, 15},  // midway in first segment
		{0.5, 20},   // at middle anchor
		{0.75, 30},  // midway in second segment
		{1.0, 40},   // at last
		{1.5, 40},   // above last → last sec
	}
	for _, c := range cases {
		if got := interpFrac(anchors, c.frac); got != c.want {
			t.Errorf("interpFrac(%.2f) = %.2f, want %.2f", c.frac, got, c.want)
		}
	}
	if got := interpFrac(nil, 0.5); got != 0 {
		t.Errorf("interpFrac(nil) = %.2f, want 0", got)
	}
}

func TestClamp01(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{{-1, 0}, {0, 0}, {0.5, 0.5}, {1, 1}, {2, 1}} {
		if got := clamp01(c.in); got != c.want {
			t.Errorf("clamp01(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
