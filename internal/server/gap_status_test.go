package server

import "testing"

// TestGapStatusModel_TightenedSemantics locks the cross-lane contract so a future
// change can't silently break the web↔mobile agreement or the tightened model.
func TestGapStatusModel_TightenedSemantics(t *testing.T) {
	m := GapStatusModelDoc()

	for _, cause := range []string{"truncated_source", "damaged_source", "dropped_segment", "unknown"} {
		if _, ok := m.Causes[cause]; !ok {
			t.Fatalf("contract missing cause %q", cause)
		}
	}

	// The tightening that matters most: dropped_segment is TEXT-only (audio plays
	// fine), so it must NOT be the "missing" level and MUST be retryable.
	drop := m.Causes["dropped_segment"]
	if drop.Level == "missing" {
		t.Fatal("dropped_segment must NOT be level 'missing' — the audio plays fine, only the text is incomplete")
	}
	if !drop.Retryable {
		t.Fatal("dropped_segment must be retryable (re-running STT recovers it)")
	}

	// truncated_source is the genuinely audio-missing case: 'missing' level, no retry.
	trunc := m.Causes["truncated_source"]
	if trunc.Level != "missing" {
		t.Fatalf("truncated_source must be level 'missing', got %q", trunc.Level)
	}
	if trunc.Retryable {
		t.Fatal("truncated_source must NOT be retryable — re-acquire, not retry")
	}

	// damaged_source shows even with no gap span, and is never retryable.
	dmg := m.Causes["damaged_source"]
	if !dmg.ShowWithoutGap {
		t.Fatal("damaged_source must render without a gap span (segment_count==0 case)")
	}
	if dmg.Retryable {
		t.Fatal("damaged_source must NOT be retryable")
	}

	// unknown is neutral (same level as dropped) but never retryable.
	unk := m.Causes["unknown"]
	if unk.Level != drop.Level {
		t.Fatalf("unknown should share the neutral level %q, got %q", drop.Level, unk.Level)
	}
	if unk.Retryable {
		t.Fatal("unknown must NEVER be retryable — never guess a retry is safe")
	}

	// Every declared cause uses a level that appears in the Levels list.
	valid := map[string]bool{}
	for _, l := range m.Levels {
		valid[l] = true
	}
	for cause, p := range m.Causes {
		if !valid[p.Level] {
			t.Fatalf("cause %q uses level %q not in Levels %v", cause, p.Level, m.Levels)
		}
	}
}

// TestGapStatusModel_LevelVocabularyStable is the drift tripwire for the one
// parallel literal the web keeps: index.html's GAP_LEVEL_CLASS maps these exact
// level tokens to CSS classes (missing→gap-missing, unverified→gap-unclassified,
// text_incomplete→gap-neutral). The contract delegates COLOUR to each client, but
// the level TOKEN VOCABULARY is shared — rename one here without updating the web
// (or bumping GapStatusModelVersion) and PJ's badge silently loses its colour.
// This fails in CI on that rename, the way transcription's contract suite catches
// a renamed key rather than letting it render wrong in the reader.
func TestGapStatusModel_LevelVocabularyStable(t *testing.T) {
	m := GapStatusModelDoc()
	// The exact tokens index.html's GAP_LEVEL_CLASS maps. Keep in lockstep.
	webMaps := map[string]bool{"missing": true, "unverified": true, "text_incomplete": true}
	if len(m.Levels) != len(webMaps) {
		t.Fatalf("level count drifted from the web's GAP_LEVEL_CLASS (%v); got %v — update index.html + bump GapStatusModelVersion", webMaps, m.Levels)
	}
	for _, l := range m.Levels {
		if !webMaps[l] {
			t.Fatalf("level %q is not mapped by the web's GAP_LEVEL_CLASS — a rename needs index.html updated + a version bump, or PJ's badge loses its colour", l)
		}
	}
}
