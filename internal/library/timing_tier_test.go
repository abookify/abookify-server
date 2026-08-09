package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestComputeTimingTier(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// A TTS narration (audio generated from the exact text).
	wid, _ := store.CreateWork("Book", "Auth")
	store.UpsertBook(db.Book{WorkID: wid, Path: "/gen/tts-fable/ch1.mp3", Filename: "ch1.mp3",
		Format: "mp3", MediaType: "audio", Origin: "tts_kokoro"})
	get := func() *db.Work { w, _ := store.GetWork(wid); return w }

	// Unprobed → UNKNOWN (never a claim we haven't earned).
	if tier, _ := ComputeTimingTier(store, get()); tier != TierUnknown {
		t.Fatalf("unprobed should be unknown, got %q", tier)
	}
	// Probed + all windows passed → verified-by-construction.
	store.UpsertTimingResult(db.TimingResult{WorkID: wid, EditionKey: "/gen/tts-fable", Windows: 3, Passed: 3})
	if tier, _ := ComputeTimingTier(store, get()); tier != TierVerifiedByConstruct {
		t.Fatalf("TTS + passed should be verified_construction, got %q", tier)
	}
	// Timing did NOT verify on every window → back to UNKNOWN, not a false claim.
	store.UpsertTimingResult(db.TimingResult{WorkID: wid, EditionKey: "/gen/tts-fable", Windows: 3, Passed: 2})
	if tier, _ := ComputeTimingTier(store, get()); tier != TierUnknown {
		t.Fatalf("partial-pass should be unknown, got %q", tier)
	}

	// A human narration, audio-only (no ebook peer), timing verified → self-consistent.
	w2, _ := store.CreateWork("Human", "Reader")
	store.UpsertBook(db.Book{WorkID: w2, Path: "/lib/human/stave1.mp3", Filename: "stave1.mp3",
		Format: "mp3", MediaType: "audio", Origin: "narrator_recording"})
	store.UpsertTimingResult(db.TimingResult{WorkID: w2, EditionKey: "/lib/human", Windows: 3, Passed: 3})
	gw2, _ := store.GetWork(w2)
	if tier, _ := ComputeTimingTier(store, gw2); tier != TierTimingSelfConsistent {
		t.Fatalf("human audio-only + passed should be self_consistent, got %q", tier)
	}
}
