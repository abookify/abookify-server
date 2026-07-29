package library

import "testing"

func words(spec ...[2]float64) []sttWord {
	var w []sttWord
	for _, s := range spec {
		w = append(w, sttWord{Word: "x", Start: s[0], End: s[1]})
	}
	return w
}

// speech fills [from,to) with one word per second.
func speech(from, to float64) []sttWord {
	var w []sttWord
	for t := from; t < to; t++ {
		w = append(w, sttWord{Word: "x", Start: t, End: t + 0.5})
	}
	return w
}

// The case the bucket detector cannot see: a 40s hole leaves enough words in
// its 60s bucket to clear the 10-word threshold, so the book reports clean.
// This is how Life of Pi hid ~1,854 words.
func TestHolesCatchSubBucketLoss(t *testing.T) {
	sc := &sttSidecar{Duration: 600, Sources: []sttSource{{Filename: "a.mp3", StartSec: 0, Duration: 600}}}
	sc.Words = append(sc.Words, speech(0, 200)...)
	sc.Words = append(sc.Words, speech(240, 600)...) // 40s hole at 200-240
	if g := DetectTranscriptionGaps(sc); len(g) != 0 {
		t.Fatalf("precondition: bucket detector should miss this, got %d gap(s)", len(g))
	}
	h := DetectTranscriptionHoles(sc)
	if len(h) != 1 {
		t.Fatalf("holes = %d, want 1 (the 40s hole)", len(h))
	}
	// The hole opens when the last word ENDS (199.5s here), not on a round
	// second, so compare with tolerance rather than exact equality.
	if h[0].StartSec < 199 || h[0].StartSec > 201 || h[0].DurationSec < 39 || h[0].DurationSec > 42 {
		t.Errorf("hole = %.1f-%.1fs (%.1fs), want ~199.5-240s (~40s)",
			h[0].StartSec, h[0].EndSec, h[0].DurationSec)
	}
}

// A run that is genuinely silent is a pause, not lost narration.
func TestHolesIgnoreRealSilence(t *testing.T) {
	sc := &sttSidecar{Duration: 600, Sources: []sttSource{{Filename: "a.mp3", StartSec: 0, Duration: 600}}}
	sc.Words = append(sc.Words, speech(0, 200)...)
	sc.Words = append(sc.Words, speech(250, 600)...)
	sc.Silences = []sttSilence{{Start: 200, End: 250, Duration: 50}}
	if h := DetectTranscriptionHoles(sc); len(h) != 0 {
		t.Errorf("silent pause reported as a hole: %+v", h)
	}
}

// Intros and outros sit at file boundaries and are not loss.
func TestHolesIgnoreShortFileEdges(t *testing.T) {
	sc := &sttSidecar{Duration: 1200, Sources: []sttSource{
		{Filename: "a.mp3", StartSec: 0, Duration: 600},
		{Filename: "b.mp3", StartSec: 600, Duration: 600},
	}}
	sc.Words = append(sc.Words, speech(0, 560)...)    // 40s outro gap at end of a.mp3
	sc.Words = append(sc.Words, speech(640, 1200)...) // 40s intro gap at start of b.mp3
	if h := DetectTranscriptionHoles(sc); len(h) != 0 {
		t.Errorf("file-edge intro/outro reported as loss: %+v", h)
	}
}

// But a LONG run reaching a file edge is exactly what a truncated file looks
// like — Oryx and Crake loses 38 minutes that way. Exempting it would hide the
// worst case in the library.
func TestHolesCatchTruncatedFileTail(t *testing.T) {
	sc := &sttSidecar{Duration: 1200, Sources: []sttSource{
		{Filename: "a.mp3", StartSec: 0, Duration: 600},
		{Filename: "b.mp3", StartSec: 600, Duration: 600},
	}}
	sc.Words = append(sc.Words, speech(0, 200)...) // audio dies at 200s, rest of file empty
	sc.Words = append(sc.Words, speech(600, 1200)...)
	h := DetectTranscriptionHoles(sc)
	if len(h) != 1 {
		t.Fatalf("holes = %d, want 1 (the truncated tail)", len(h))
	}
	if h[0].DurationSec < 350 {
		t.Errorf("truncated tail reported as %.0fs, want ~400s", h[0].DurationSec)
	}
}

// Overlapping findings must not double-count: the total feeds a "N minutes
// missing" figure read literally by the user.
func TestMergeGapSpansDropsOverlaps(t *testing.T) {
	gaps := []TranscriptionGap{{StartSec: 100, EndSec: 200, DurationSec: 100}}
	holes := []TranscriptionGap{
		{StartSec: 150, EndSec: 190, DurationSec: 40}, // inside the gap
		{StartSec: 400, EndSec: 460, DurationSec: 60}, // distinct
	}
	out := mergeGapSpans(gaps, holes)
	if len(out) != 2 {
		t.Fatalf("merged = %d span(s), want 2", len(out))
	}
	var total float64
	for _, g := range out {
		total += g.DurationSec
	}
	if total != 160 {
		t.Errorf("total = %.0fs, want 160 (overlap counted once)", total)
	}
	if out[0].StartSec > out[1].StartSec {
		t.Error("merged spans are not ordered by start time")
	}
}
