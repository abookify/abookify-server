package library

import (
	"testing"
)

func TestSummarizeDivergence(t *testing.T) {
	cases := []struct {
		name     string
		total    int
		covered  int
		ratio    float64
		wantHint string
	}{
		{"empty", 0, 0, 0, "no paragraphs"},
		{"full", 100, 100, 1.0, "full coverage"},
		{"near", 100, 90, 0.90, "near-complete"},
		{"partial", 100, 70, 0.70, "partial coverage"},
		{"low", 100, 20, 0.20, "low coverage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &DivergenceReport{TotalParagraphs: c.total, CoveredParagraphs: c.covered, CoverageRatio: c.ratio}
			got := summarizeDivergence(r)
			if !contains(got, c.wantHint) {
				t.Errorf("summary=%q, want substring %q", got, c.wantHint)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
