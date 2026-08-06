// Edition verdict: is this ebook↔transcript pair the same edition, the same
// work in a different edition/translation, or not the same text at all?
//
// Users pair whatever audiobook and ebook they happen to own, so mismatched
// editions are the NORMAL case, not an edge case. A coverage percentage alone
// invites the wrong conclusion: Republic reads "0.2%" against a complete-
// dialogues EPUB while being a perfectly healthy pairing of one dialogue.
// The verdict names the situation instead; the raw numbers ride along so a
// badge can be audited later without re-running alignment.
//
// The verdict is derived ONLY from stored alignment rows — the anchor row's
// directional quality and, when present, the embedding row's mean matched
// cosine + matched-narration share. No recomputation happens at read time.
package library

// Thresholds, calibrated on the 2026-08-06 library sweep (19 works measured,
// values recorded in engineering/handoff/transcription.md). Each sits inside a
// measured empty gap, not at a guess:
//
//   - verdictAnchorQualityHigh (0.85): same-edition pairs measured >= 0.937
//     (Heart of Darkness, 48 Laws, Why We Sleep, Farewell to Arms) while every
//     cross-edition pair measured <= 0.608 (Hero) — a 33-point empty gap.
//   - verdictEmbSameWork (0.75): the embedding space puts same-passage
//     paraphrases at ~0.75-0.95 (see embSimThreshold). Measured same-work
//     pairs: 0.77 (Meditations) to 0.92; the one known not-same-text pair
//     (Owl Creek radio DRAMA vs the short story) measured 0.686. The margin
//     to Meditations is only 0.084 — the narrowest gate here — which is why
//     NearBoundary exists and why a verdict close to it must render softly.
//   - verdictMatchedShareLow (0.50): within different_edition, separates
//     "approximate read-along is plausible" (measured >= 0.597, Life of Pi)
//     from "most of the narration has no counterpart in this ebook edition"
//     (Gulag: 0.286 — the Vintage ebook is abridged; the audio is fuller).
//
// Life of Pi (share 0.597) and Meditations (embq 0.77) are the two works
// nearest a boundary; both carry NearBoundary=true.
const (
	verdictAnchorQualityHigh = 0.85
	verdictEmbSameWork       = 0.75
	verdictMatchedShareLow   = 0.50
	// verdictNearMargin flags a verdict whose deciding comparison fell within
	// this distance of its threshold: true means "one small re-alignment could
	// flip this bucket", and the UI should not present it as a hard fact.
	verdictNearMargin = 0.05
)

// Verdict buckets.
const (
	VerdictSameEdition      = "same_edition"      // lexical alignment is trustworthy; read SCOPE for how much is narrated
	VerdictDifferentEdition = "different_edition" // same work, different translation/edition — read-along will be approximate
	VerdictDifferentText    = "different_text"    // these do not appear to share text (adaptation or wrong pairing)
	VerdictUnknown          = "unknown"           // lexical alignment failed and no embedding comparison exists yet
)

// EditionVerdict is the classification plus the raw numbers it was derived
// from, so consumers render without re-deriving and audits don't re-align.
type EditionVerdict struct {
	Bucket string `json:"bucket"`
	Detail string `json:"detail"`
	// AnchorQuality is the anchor row's audio→ebook ratio: how much of the
	// narration is backed lexically by this ebook's text.
	AnchorQuality float64 `json:"anchor_quality"`
	// EmbeddingSimilarity is the embedding row's mean matched-pair cosine
	// (match_quality). Zero when no embedding row exists.
	EmbeddingSimilarity float64 `json:"embedding_similarity,omitempty"`
	// MatchedNarrationShare is the embedding row's audio→ebook ratio at
	// paragraph level: the share of narration matched to this ebook at all.
	MatchedNarrationShare float64 `json:"matched_narration_share,omitempty"`
	// NearBoundary: the deciding comparison fell within verdictNearMargin of
	// its threshold — present this verdict softly, it could flip on a
	// re-alignment.
	NearBoundary bool `json:"near_boundary,omitempty"`
}

func near(v, threshold float64) bool {
	d := v - threshold
	if d < 0 {
		d = -d
	}
	return d <= verdictNearMargin
}

// computeEditionVerdict classifies one pair. emb is nil when the pair has no
// embedding row (embq/embShare are then ignored).
func computeEditionVerdict(anchorQuality float64, hasEmb bool, embq, embShare float64) *EditionVerdict {
	v := &EditionVerdict{AnchorQuality: anchorQuality}
	if hasEmb {
		v.EmbeddingSimilarity = embq
		v.MatchedNarrationShare = embShare
	}

	if anchorQuality >= verdictAnchorQualityHigh {
		v.Bucket = VerdictSameEdition
		v.Detail = "narration matches this ebook's text; the scope column shows how much of the ebook is narrated"
		v.NearBoundary = near(anchorQuality, verdictAnchorQualityHigh)
		return v
	}
	if !hasEmb {
		v.Bucket = VerdictUnknown
		v.Detail = "lexical alignment is low and no embedding comparison exists for this pair yet"
		v.NearBoundary = near(anchorQuality, verdictAnchorQualityHigh)
		return v
	}
	if embq >= verdictEmbSameWork {
		v.Bucket = VerdictDifferentEdition
		if embShare < verdictMatchedShareLow {
			v.Detail = "same work, but most of the narration has no counterpart in this ebook edition (abridged or partial edition)"
		} else {
			v.Detail = "same work in a different edition or translation — read-along will be approximate"
		}
		v.NearBoundary = near(embq, verdictEmbSameWork) || near(embShare, verdictMatchedShareLow)
		return v
	}
	v.Bucket = VerdictDifferentText
	v.Detail = "these sources do not appear to share text — an adaptation, or possibly the wrong pairing"
	v.NearBoundary = near(embq, verdictEmbSameWork)
	return v
}
