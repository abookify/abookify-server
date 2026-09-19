package library

import "testing"

// The Settings UI's pause values must reach the assembly: a setting nobody
// reads is decoration. Supplied pauses land on the segments; non-positive
// values fall back to the PAUSED defaults.
func TestPreprocessForTTSSegmentsWithPauses(t *testing.T) {
	segs := PreprocessForTTSSegmentsWithPauses("Stave One", "Marley was dead: to begin with.\n\nThere is no doubt whatever about that.\n\nThe register was signed.", 900, 300)
	if len(segs) != 4 {
		t.Fatalf("segments = %d, want 4 (title + 3 paragraphs): %+v", len(segs), segs)
	}
	if segs[0].PauseAfterMs != 900 || segs[1].PauseAfterMs != 300 || segs[2].PauseAfterMs != 300 || segs[3].PauseAfterMs != 0 {
		t.Errorf("pauses = %d/%d/%d/%d, want 900/300/300/0", segs[0].PauseAfterMs, segs[1].PauseAfterMs, segs[2].PauseAfterMs, segs[3].PauseAfterMs)
	}
	def := PreprocessForTTSSegmentsWithPauses("Stave One", "Marley was dead.\n\nNo doubt.", 0, -5)
	if def[0].PauseAfterMs != ttsTitlePauseMs || def[1].PauseAfterMs != ttsParagraphPauseMs {
		t.Errorf("non-positive pauses must fall back to the defaults: %+v", def)
	}
}
