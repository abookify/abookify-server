package server

import "testing"

// The cause↔legend contract, locked.
//
// Two lanes meet here: transcription decides which `cause` a work gets
// (handleTranscriptionGapsSummary), and web/mobile decide how each renders
// (GapStatusModelDoc). Nothing but agreement of string literals binds them, and
// that exact shape of coupling already cost a reconciliation commit today — the
// /unload endpoint returned {"was_loaded":bool} while the Go client parsed
// {"unloaded":bool}, so unloads silently reported freed=false.
//
// These assertions are about MEANING, not spelling. A renamed key fails loudly
// here instead of rendering a blank badge in the reader.

// causesEmitted lists every value handleTranscriptionGapsSummary can set. Keep
// in step with that switch; the test below is what makes a divergence visible.
var causesEmitted = []string{
	"dropped_segment",
	"truncated_source",
	"damaged_source",
	"unknown",
}

func TestEveryEmittedCauseHasLegendEntry(t *testing.T) {
	legend := GapStatusModelDoc()
	for _, c := range causesEmitted {
		if _, ok := legend.Causes[c]; !ok {
			t.Errorf("cause %q is emitted by the summary but absent from the legend — "+
				"the UI has nothing to render for it", c)
		}
	}
}

func TestLegendHasNoOrphanCauses(t *testing.T) {
	legend := GapStatusModelDoc()
	emitted := map[string]bool{}
	for _, c := range causesEmitted {
		emitted[c] = true
	}
	for c := range legend.Causes {
		if !emitted[c] {
			t.Errorf("legend defines %q but the summary never emits it — either dead "+
				"presentation or a cause the server forgot to set", c)
		}
	}
}

// The invariant that stops the Retry button lying. 187 minutes of PJ's library is
// fixed by re-running STT and 74 minutes is not; offering one Retry across both
// spends an hour of GPU to change nothing.
func TestOnlyDroppedSegmentIsRetryable(t *testing.T) {
	for cause, p := range GapStatusModelDoc().Causes {
		wantRetry := cause == "dropped_segment"
		if p.Retryable != wantRetry {
			t.Errorf("cause %q retryable=%v, want %v — a retry is only honest when the "+
				"SOURCE is intact and transcription dropped the text", cause, p.Retryable, wantRetry)
		}
	}
}

// damaged_source must render with no gap span at all. That is the Life of Pi
// case: ~1,854 words vanished below the gap threshold, so the book reported
// clean. If this stops showing without a gap, silent loss becomes invisible again.
func TestDamagedSourceShowsWithoutGap(t *testing.T) {
	p, ok := GapStatusModelDoc().Causes["damaged_source"]
	if !ok {
		t.Fatal("damaged_source missing from the legend")
	}
	if !p.ShowWithoutGap {
		t.Error("damaged_source must render with segment_count == 0 — words can be lost " +
			"without ever crossing the gap threshold")
	}
}

// truncated_source must not read as a transcription problem, and
// dropped_segment must not read as the recording being short. The audio is
// intact in one case and genuinely missing in the other.
func TestCauseLevelsMatchWhichArtefactIsDamaged(t *testing.T) {
	legend := GapStatusModelDoc()
	if got := legend.Causes["truncated_source"].Level; got != "missing" {
		t.Errorf("truncated_source level = %q, want \"missing\" — the recording really is short", got)
	}
	if got := legend.Causes["dropped_segment"].Level; got == "missing" {
		t.Error("dropped_segment must NOT use the \"missing\" level — the audio plays " +
			"perfectly and only the transcript is incomplete")
	}
	if got := legend.Causes["truncated_source"].Action; got != "reacquire" {
		t.Errorf("truncated_source action = %q, want \"reacquire\" — re-running STT cannot "+
			"recover audio that was never delivered", got)
	}
}

// Every cause's level must be one the UI knows how to style.
func TestCauseLevelsAreDeclared(t *testing.T) {
	legend := GapStatusModelDoc()
	known := map[string]bool{}
	for _, l := range legend.Levels {
		known[l] = true
	}
	for cause, p := range legend.Causes {
		if !known[p.Level] {
			t.Errorf("cause %q uses level %q which is not in Levels %v", cause, p.Level, legend.Levels)
		}
	}
}
