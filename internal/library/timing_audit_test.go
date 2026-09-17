package library

import "testing"

// The drift's smallest live case (Frankenstein, 43 s at the end) must fail;
// the healthy shape (0–7 s) must pass; an ambiguous set is a skip, never a
// guess.
func TestTimingReport_Summarize(t *testing.T) {
	healthy := TimingReport{Deltas: []ChapterTimingDelta{{DeltaSec: 3.9}, {DeltaSec: -4.4}, {DeltaSec: 1.2}, {DeltaSec: -4.4}}}
	healthy.summarize()
	if !healthy.OK || healthy.Skipped != "" || healthy.Within != 4 {
		t.Fatalf("healthy = %+v", healthy)
	}
	// Gradual drift: early chapters fine, the back half 40–90 s off. The
	// median alone would pass it; the within-tolerance share fails it.
	gradual := TimingReport{Deltas: []ChapterTimingDelta{{DeltaSec: 2}, {DeltaSec: 5}, {DeltaSec: 12}, {DeltaSec: 24}, {DeltaSec: 41}, {DeltaSec: 63}, {DeltaSec: 90}}}
	gradual.summarize()
	if gradual.OK || gradual.Within != 4 {
		t.Fatalf("gradual drift passed: %+v", gradual)
	}
	// One outlier (an ebook unit the narration never announces) among many
	// on-the-mark chapters is listed, not failed.
	outlier := TimingReport{Deltas: []ChapterTimingDelta{{DeltaSec: 0}, {DeltaSec: 2}, {DeltaSec: 0}, {DeltaSec: 6}, {TextTitle: "Appendix", DeltaSec: -226.8}}}
	outlier.summarize()
	if !outlier.OK || outlier.WorstTitle != "Appendix" {
		t.Fatalf("outlier = %+v", outlier)
	}
	thin := TimingReport{Deltas: []ChapterTimingDelta{{DeltaSec: 900}, {DeltaSec: 900}}}
	thin.summarize()
	if thin.OK || thin.Skipped == "" {
		t.Fatalf("two chapters must not yield a verdict: %+v", thin)
	}
}

// Whisper stretched the body's first word over the pause, so the spoken title
// carries one stray word. The publisher's title for the same number decides.
func TestTrimBleedAgainst(t *testing.T) {
	pub := map[int]string{
		8:  "Battle of the generations",
		12: "Nice guys finish first",
		1:  "Why are people?",
		5:  "", // ambiguous numbering on the publisher side → never trim
	}
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"Chapter 8: Battle of the Generations Let", "Chapter 8: Battle of the Generations", true},
		{"Chapter 12: Nice Guys Finish First Nice", "Chapter 12: Nice Guys Finish First", true},
		{"12. Nice Guys Finish First Nice", "12. Nice Guys Finish First", true},
		{"Chapter 1: Why Are People", "", false},                         // already agrees
		{"Chapter 8: Something else entirely here", "", false},           // different title
		{"Chapter 8: Battle of the Generations Let us begin", "", false}, // 3 extra: not bleed, a real cut failure — leave it
		{"Chapter 5: Aggression stability and", "", false},               // publisher ambiguous
		{"Chapter 9", "", false}, // no subtitle
		{"Prelude", "", false},
	}
	for _, c := range cases {
		got, ok := trimBleedAgainst(c.in, pub)
		if ok != c.ok || got != c.want {
			t.Errorf("%q → (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
