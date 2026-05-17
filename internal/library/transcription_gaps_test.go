package library

import "testing"

// Empty sidecar (or shorter than one bucket) returns no gaps.
func TestDetectTranscriptionGaps_TinyBook(t *testing.T) {
	sc := &sttSidecar{Duration: 30, Words: []sttWord{{Start: 0, End: 1, Word: "a"}}}
	if got := DetectTranscriptionGaps(sc); len(got) != 0 {
		t.Errorf("want 0 gaps for tiny book, got %d: %+v", len(got), got)
	}
}

// A 5-minute book with steady narration throughout has no gaps. ~150
// wpm means ~150 words in each 60s bucket — well above the
// minWordsPerBucket threshold.
func TestDetectTranscriptionGaps_HealthyTranscript(t *testing.T) {
	var words []sttWord
	for i := 0; i < 750; i++ { // 750 words across 300s = 150 wpm
		t := float64(i) * 0.4
		words = append(words, sttWord{Start: t, End: t + 0.3, Word: " w"})
	}
	sc := &sttSidecar{Duration: 300, Words: words}
	got := DetectTranscriptionGaps(sc)
	if len(got) != 0 {
		t.Errorf("want 0 gaps for healthy book, got %d: %+v", len(got), got)
	}
}

// Whisper produced words for 0-60s and 240-300s but nothing in
// between. The 180-second middle stretch is a clear gap.
func TestDetectTranscriptionGaps_MiddleHole(t *testing.T) {
	var words []sttWord
	for i := 0; i < 150; i++ { // bucket 0: dense
		t := float64(i) * 0.4
		words = append(words, sttWord{Start: t, End: t + 0.3, Word: " w"})
	}
	for i := 0; i < 150; i++ { // bucket 4: dense
		t := 240.0 + float64(i)*0.4
		words = append(words, sttWord{Start: t, End: t + 0.3, Word: " w"})
	}
	sc := &sttSidecar{Duration: 300, Words: words}
	gaps := DetectTranscriptionGaps(sc)
	if len(gaps) != 1 {
		t.Fatalf("want 1 gap, got %d: %+v", len(gaps), gaps)
	}
	g := gaps[0]
	if g.StartSec != 60 || g.EndSec != 240 || g.WordCount != 0 {
		t.Errorf("unexpected gap: %+v", g)
	}
}

// A chapter-grade silence in the middle of the book is NOT a gap —
// the audio there is intentionally quiet, no transcription was
// possible. Silence coverage > 75% of the bucket suppresses the flag.
func TestDetectTranscriptionGaps_SilenceIsNotAGap(t *testing.T) {
	var words []sttWord
	for i := 0; i < 150; i++ { // 0-60s populated
		t := float64(i) * 0.4
		words = append(words, sttWord{Start: t, End: t + 0.3, Word: " w"})
	}
	for i := 0; i < 150; i++ { // 120-180s populated
		t := 120.0 + float64(i)*0.4
		words = append(words, sttWord{Start: t, End: t + 0.3, Word: " w"})
	}
	// 60-120s: long acoustic silence covers most of the bucket.
	sils := []sttSilence{
		{Start: 60, End: 118, Duration: 58, Kind: "chapter"},
	}
	sc := &sttSidecar{Duration: 180, Words: words, Silences: sils}
	if got := DetectTranscriptionGaps(sc); len(got) != 0 {
		t.Errorf("silence shouldn't be a gap, got %+v", got)
	}
}

// Source-file lookup: a gap that lands inside a missing file should be
// tagged with that file in the report. Models Bonfire's shape — files
// 12 and 15 transcribed normally, files 13 and 14 dropped — but at
// reduced scale (60s/file instead of 3600s) to keep the test data
// cheap. The 120s gap covering files 13+14 should tag "13.mp3" since
// that's the file at the gap's start time.
func TestDetectTranscriptionGaps_TagsSourceFile(t *testing.T) {
	dense := func(start float64, count int) []sttWord {
		out := make([]sttWord, count)
		for i := 0; i < count; i++ {
			t := start + float64(i)*0.4
			out[i] = sttWord{Start: t, End: t + 0.3, Word: " w"}
		}
		return out
	}
	var words []sttWord
	words = append(words, dense(0, 150)...)   // file 12 (0-60)
	words = append(words, dense(180, 150)...) // file 15 (180-240)
	sources := []sttSource{
		{Filename: "12.mp3", StartSec: 0, Duration: 60},
		{Filename: "13.mp3", StartSec: 60, Duration: 60},
		{Filename: "14.mp3", StartSec: 120, Duration: 60},
		{Filename: "15.mp3", StartSec: 180, Duration: 60},
	}
	sc := &sttSidecar{Duration: 240, Words: words, Sources: sources}
	gaps := DetectTranscriptionGaps(sc)
	if len(gaps) != 1 {
		t.Fatalf("want 1 merged gap (files 13-14), got %d: %+v", len(gaps), gaps)
	}
	g := gaps[0]
	if g.StartSec != 60 || g.EndSec != 180 {
		t.Errorf("gap span wrong: got %.0f-%.0f, want 60-180", g.StartSec, g.EndSec)
	}
	if g.SourceFile != "13.mp3" {
		t.Errorf("gap should tag start file (13.mp3), got %q", g.SourceFile)
	}
}
