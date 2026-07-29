package main

import (
	"sort"
	"strings"
	"testing"
)

// narration builds one word per 0.4s (150 wpm) over [from,to).
func narration(from, to float64) []wordTS {
	var w []wordTS
	for t := from; t < to; t += 0.4 {
		w = append(w, wordTS{Word: "x", Start: t, End: t + 0.3})
	}
	return w
}

// A sound sidecar must not be flagged. False alarms here would train the
// operator to ignore the one that matters.
func TestIntegrityCleanSidecar(t *testing.T) {
	w := narration(0, 1200)
	src := []sourceInfo{
		{Filename: "a.mp3", StartSec: 0, Duration: 600},
		{Filename: "b.mp3", StartSec: 600, Duration: 600},
	}
	if p := checkSidecarIntegrity(w, src, 1200); len(p) != 0 {
		t.Errorf("clean sidecar flagged: %v", p)
	}
}

// Gross duplication — several transcriptions stacked on one stretch — is caught.
func TestIntegrityCatchesGrossDuplication(t *testing.T) {
	w := narration(0, 1200)
	for i := 0; i < 3; i++ { // 4x the words over 300-500s
		w = append(w, narration(300, 500)...)
	}
	sort.Slice(w, func(i, j int) bool { return w[i].Start < w[j].Start })

	p := checkSidecarIntegrity(w, nil, 1200)
	if len(p) == 0 {
		t.Fatal("4x duplication not detected")
	}
	if !strings.Contains(strings.Join(p, " "), "words/min") {
		t.Errorf("density problem not reported: %v", p)
	}
}

// Documents the LIMIT rather than pretending it away: a simple doubling is
// invisible to any density threshold, because legitimate books reach 2.31x their
// own median 60s rate (measured across this library), so 2x normal narration is
// inside the normal range. The defence for that case is verifyAgainstSidecar,
// which rejects the mismatched input before the words are ever written.
func TestIntegrityDensityCannotCatchSimpleDoubling(t *testing.T) {
	w := narration(0, 1200)
	w = append(w, narration(300, 500)...) // 2x on that stretch = 300 wpm
	sort.Slice(w, func(i, j int) bool { return w[i].Start < w[j].Start })

	for _, p := range checkSidecarIntegrity(w, nil, 1200) {
		if strings.Contains(p, "words/min") {
			t.Errorf("density fired on a plain doubling (%s) — if this now works, "+
				"tighten the documented scope; if it false-fires on real books, loosen it", p)
		}
	}
}

// Words outside the audio are a misplacement that overshot the end.
func TestIntegrityCatchesOutOfRange(t *testing.T) {
	w := narration(0, 600)
	w = append(w, wordTS{Word: "late", Start: 5000, End: 5001})
	p := checkSidecarIntegrity(w, nil, 600)
	if len(p) == 0 {
		t.Fatal("word beyond the audio duration not detected")
	}
	if !strings.Contains(strings.Join(p, " "), "outside the audio") {
		t.Errorf("unexpected problem text: %v", p)
	}
}

// A hole or overlap in the sources array means the timeline no longer tiles —
// the exact symptom of a stray file joining the input set.
func TestIntegrityCatchesSourceDiscontinuity(t *testing.T) {
	w := narration(0, 1200)
	src := []sourceInfo{
		{Filename: "a.mp3", StartSec: 0, Duration: 600},
		{Filename: "b.mp3", StartSec: 2140, Duration: 600}, // pushed 1540s late
	}
	p := checkSidecarIntegrity(w, src, 2740)
	if len(p) == 0 {
		t.Fatal("discontinuous sources not detected")
	}
	if !strings.Contains(strings.Join(p, " "), "b.mp3") {
		t.Errorf("problem does not name the offending source: %v", p)
	}
}

// Ordinary fast narration must not trip the density ceiling.
func TestIntegrityToleratesFastNarration(t *testing.T) {
	var w []wordTS
	for t0 := 0.0; t0 < 600; t0 += 0.30 { // 200 wpm, the fastest in this library
		w = append(w, wordTS{Word: "x", Start: t0, End: t0 + 0.2})
	}
	if p := checkSidecarIntegrity(w, nil, 600); len(p) != 0 {
		t.Errorf("200 wpm narration flagged: %v", p)
	}
}

// An empty sidecar is not a corruption; other checks report that.
func TestIntegrityEmptyWords(t *testing.T) {
	if p := checkSidecarIntegrity(nil, nil, 600); len(p) != 0 {
		t.Errorf("empty word list flagged: %v", p)
	}
}

// Collapsed word timings mean the timestamps were synthesized from a
// segment-level result rather than measured. Heart Goes Last's re-transcription
// wrote 151 words into a 31s span with up to 31 words on ONE timestamp, and
// prose that did not match the audio — a word count that went UP while the
// content went wrong. This is the check that catches that without reading the
// text by hand.
func TestIntegrityCatchesCollapsedTimestamps(t *testing.T) {
	w := narration(0, 600)
	for i := 0; i < 20; i++ { // 20 words all claiming the same instant
		w = append(w, wordTS{Word: "x", Start: 300.0, End: 300.4})
	}
	sort.Slice(w, func(i, j int) bool { return w[i].Start < w[j].Start })

	p := checkSidecarIntegrity(w, nil, 600)
	if len(p) == 0 {
		t.Fatal("collapsed word timings not detected")
	}
	if !strings.Contains(strings.Join(p, " "), "synthesized") {
		t.Errorf("collapse not reported: %v", p)
	}
}

// Genuine word timings occasionally coincide; a couple sharing an instant is
// not corruption.
func TestIntegrityToleratesIncidentalTies(t *testing.T) {
	w := narration(0, 600)
	w = append(w, wordTS{Word: "a", Start: 100.0, End: 100.2},
		wordTS{Word: "b", Start: 100.0, End: 100.2})
	sort.Slice(w, func(i, j int) bool { return w[i].Start < w[j].Start })
	for _, p := range checkSidecarIntegrity(w, nil, 600) {
		if strings.Contains(p, "synthesized") {
			t.Errorf("two coincident words flagged as collapse: %s", p)
		}
	}
}
