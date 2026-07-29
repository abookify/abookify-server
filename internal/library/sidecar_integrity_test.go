package library

import (
	"strings"
	"testing"
)

func narrationWords(from, to float64) []sttWord {
	var w []sttWord
	for t := from; t < to; t += 0.4 { // 150 wpm
		w = append(w, sttWord{Word: "x", Start: t, End: t + 0.3})
	}
	return w
}

func kinds(ps []SidecarProblem) string {
	var k []string
	for _, p := range ps {
		k = append(k, p.Kind)
	}
	return strings.Join(k, ",")
}

// A sound sidecar must pass. False alarms on import would train whoever reads
// the log to ignore the one that matters.
func TestImportIntegrityCleanSidecar(t *testing.T) {
	sc := &sttSidecar{Duration: 1200,
		Words: narrationWords(0, 1200),
		Sources: []sttSource{
			{Filename: "a.mp3", StartSec: 0, Duration: 600},
			{Filename: "b.mp3", StartSec: 600, Duration: 600},
		}}
	if p := checkSidecarIntegrity(sc); len(p) != 0 {
		t.Errorf("clean sidecar flagged: %s", kinds(p))
	}
}

// THE case this exists for: fabricated text arrives with collapsed word timings,
// and every other signal reads normal. This is how ~136,000 words of invented
// prose sat in the library looking like complete books.
func TestImportIntegrityCatchesSynthesizedTimings(t *testing.T) {
	sc := &sttSidecar{Duration: 600, Words: narrationWords(0, 600)}
	for i := 0; i < 20; i++ { // 20 words all claiming one instant
		sc.Words = append(sc.Words, sttWord{Word: "x", Start: 300.0, End: 300.4})
	}
	p := checkSidecarIntegrity(sc)
	if len(p) == 0 {
		t.Fatal("collapsed timings not detected on import")
	}
	if !strings.Contains(kinds(p), "synthesized_word_timings") {
		t.Errorf("wrong problem kind: %s", kinds(p))
	}
	var found *SidecarProblem
	for i := range p {
		if p[i].Kind == "synthesized_word_timings" {
			found = &p[i]
		}
	}
	if found.Count < 20 {
		t.Errorf("affected count = %d, want >= 20", found.Count)
	}
}

// A few coincident timings are normal rounding, not corruption.
func TestImportIntegrityToleratesTies(t *testing.T) {
	sc := &sttSidecar{Duration: 600, Words: narrationWords(0, 600)}
	for i := 0; i < 4; i++ {
		sc.Words = append(sc.Words, sttWord{Word: "x", Start: 100.0, End: 100.2})
	}
	if strings.Contains(kinds(checkSidecarIntegrity(sc)), "synthesized") {
		t.Error("four coincident words flagged as synthesized")
	}
}

func TestImportIntegrityCatchesOutOfRangeAndDiscontinuity(t *testing.T) {
	sc := &sttSidecar{Duration: 600, Words: append(narrationWords(0, 600),
		sttWord{Word: "late", Start: 5000, End: 5001})}
	if !strings.Contains(kinds(checkSidecarIntegrity(sc)), "words_outside_audio") {
		t.Error("word beyond the audio not detected")
	}

	// A stray file joining the set pushes later sources down the timeline — the
	// exact shape of the misplacement that reported "+13,619 words recovered".
	sc2 := &sttSidecar{Duration: 2740, Words: narrationWords(0, 1200),
		Sources: []sttSource{
			{Filename: "a.mp3", StartSec: 0, Duration: 600},
			{Filename: "b.mp3", StartSec: 2140, Duration: 600},
		}}
	p := checkSidecarIntegrity(sc2)
	if !strings.Contains(kinds(p), "source_discontinuity") {
		t.Errorf("discontinuous sources not detected: %s", kinds(p))
	}
}

// Ordinary fast narration must not trip the density backstop; 200 wpm is the
// fastest real narration in this library.
func TestImportIntegrityToleratesFastNarration(t *testing.T) {
	var w []sttWord
	for t0 := 0.0; t0 < 600; t0 += 0.30 {
		w = append(w, sttWord{Word: "x", Start: t0, End: t0 + 0.2})
	}
	sc := &sttSidecar{Duration: 600, Words: w}
	if strings.Contains(kinds(checkSidecarIntegrity(sc)), "implausible_density") {
		t.Error("200 wpm narration flagged as implausible")
	}
}

func TestImportIntegrityEmptyIsNotAProblem(t *testing.T) {
	if p := checkSidecarIntegrity(&sttSidecar{Duration: 600}); len(p) != 0 {
		t.Errorf("empty sidecar flagged: %s", kinds(p))
	}
}

// The in-app Transcribe job writes word timings straight to the database with no
// sidecar, so it was the one path with NO integrity validation while identical
// work through stt-cli got two checks. This is that path's guard.
func TestCheckTranscriptIntegrityCatchesFabricatedResult(t *testing.T) {
	// A fresh result carrying the fabricated-text signature: many words claiming
	// one instant, exactly as faster-whisper returns when its word-alignment pass
	// collapses.
	words := narrationWords(0, 600)
	for i := 0; i < 25; i++ {
		words = append(words, sttWord{Word: "x", Start: 120.0, End: 120.3})
	}
	p := CheckTranscriptIntegrity(words, nil, 600)
	if len(p) == 0 {
		t.Fatal("fabricated-result signature not detected on the server STT path")
	}
	if !strings.Contains(kinds(p), "synthesized_word_timings") {
		t.Errorf("wrong kind: %s", kinds(p))
	}
}

// A normal transcription must pass — this runs on every in-app job, so a false
// alarm would cry wolf on every book PJ transcribes from the UI.
func TestCheckTranscriptIntegrityPassesNormalResult(t *testing.T) {
	if p := CheckTranscriptIntegrity(narrationWords(0, 1800), nil, 1800); len(p) != 0 {
		t.Errorf("normal transcription flagged: %s", kinds(p))
	}
}

// Reporting returns a count so a caller can act on it, and stays silent on a
// clean result.
func TestLogTranscriptProblemsCounts(t *testing.T) {
	if n := LogTranscriptProblems("clean.mp3", narrationWords(0, 600), nil, 600); n != 0 {
		t.Errorf("clean result reported %d problem(s)", n)
	}
	bad := narrationWords(0, 600)
	for i := 0; i < 25; i++ {
		bad = append(bad, sttWord{Word: "x", Start: 50.0, End: 50.2})
	}
	if n := LogTranscriptProblems("bad.mp3", bad, nil, 600); n == 0 {
		t.Error("fabricated result reported no problems")
	}
}
