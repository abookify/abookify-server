package library

import (
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// Timing trust tiers (engineering-level DATA; the reader-facing LABEL is a
// separate product-voice decision drafted for PJ). Each says what it knows and,
// by omission, what it doesn't. An empty tier = NOT YET CHECKED = unknown, which
// must never render as a verified claim (the nil-able rule).
const (
	TierUnknown              = ""                       // probe hasn't run, or timing didn't verify
	TierVerifiedByConstruct  = "verified_construction"  // our TTS: audio generated from the exact text + timing verified
	TierTimingTextVerified   = "timing_text_verified"   // human narration + aligned ebook peer, timing + text both verified
	TierTimingSelfConsistent = "timing_self_consistent" // audio-only / no independent text, timing verified
)

// ComputeTimingTier derives the trust tier for a work's DISPLAYED audio edition
// from three things: whether the timing probe VERIFIED it, the narration's
// PROVENANCE (our TTS has independent ground truth; a human narration doesn't),
// and whether an aligned EBOOK PEER exists to cross-check the text. Returns
// TierUnknown ("") when the edition hasn't been probed or didn't verify — a claim
// we can't stand behind is never rendered.
func ComputeTimingTier(store *db.Store, work *db.Work) (string, error) {
	disp := ResolveDisplayAudio(work)
	if disp == nil {
		return TierUnknown, nil // no audio → no read-along tier
	}
	editionKey := filepath.Dir(disp.Path)

	results, err := store.GetTimingResults(work.ID)
	if err != nil {
		return TierUnknown, err
	}
	res, ok := results[editionKey]
	if !ok || res.Windows == 0 || res.Passed < res.Windows {
		// Not probed, or the timing did NOT verify on every window. Either way we
		// have not earned a claim — report unknown, never a false "verified".
		return TierUnknown, nil
	}

	// Timing verified. Our own narration is generated from the exact text, so the
	// check has independent ground truth — the strongest, verified-by-construction.
	if strings.HasPrefix(disp.Origin, "tts_") {
		return TierVerifiedByConstruct, nil
	}

	// Human narration. It is text-verified only when an aligned ebook peer exists
	// to cross-check the words against (an independent text). Note: this asserts
	// the highlighting lines up and the narration matches the book's text — NOT
	// that the human narrator is flawless.
	hasEbook := false
	for _, b := range work.TextFiles {
		if b.Format == "epub" {
			hasEbook = true
			break
		}
	}
	hasWordAlign := false
	if aligns, err := store.ListAlignmentsForWork(work.ID); err == nil {
		for _, a := range aligns {
			if a.Unit == "word" {
				hasWordAlign = true
				break
			}
		}
	}
	if hasEbook && hasWordAlign {
		return TierTimingTextVerified, nil
	}
	return TierTimingSelfConsistent, nil
}
