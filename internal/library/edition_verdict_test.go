package library

import "testing"

// Every case here is a REAL measured pairing from the 2026-08-06 library sweep
// (values in the handoff), so a threshold change that flips a known-true
// verdict fails loudly with the book named.
func TestComputeEditionVerdictOnMeasuredLibrary(t *testing.T) {
	cases := []struct {
		name        string
		anchorQ     float64
		hasEmb      bool
		embq, share float64
		wantBucket  string
		wantNearity bool
	}{
		// Same edition, partial narration — collections/abridged audio.
		{"Heart of Darkness (collection ebook)", 0.937, true, 0.861, 0.909, VerdictSameEdition, false},
		{"48 Laws (concise audio)", 0.956, true, 0.863, 0.885, VerdictSameEdition, false},
		{"Why We Sleep (partial audio)", 0.953, true, 0.922, 0.954, VerdictSameEdition, false},

		// Same work, different translation/edition.
		{"Meditations (Long audio vs other translation)", 0.049, true, 0.774, 0.930, VerdictDifferentEdition, true}, // embq 0.774 is 0.024 from the gate
		{"Republic (one dialogue vs complete Plato)", 0.040, true, 0.840, 0.719, VerdictDifferentEdition, false},
		{"Iliad (different translation)", 0.031, true, 0.850, 0.841, VerdictDifferentEdition, false},
		{"Hero With a Thousand Faces (edition variance)", 0.608, true, 0.870, 0.637, VerdictDifferentEdition, false},
		{"Life of Pi (share 0.597 near the 0.5 gate)", 0.547, true, 0.870, 0.597, VerdictDifferentEdition, false},

		// Same work but the ebook edition is missing most of what's narrated.
		{"Gulag (abridged Vintage ebook vs fuller audio)", 0.257, true, 0.831, 0.286, VerdictDifferentEdition, false},

		// Not the same text.
		{"Owl Creek (radio drama vs short story)", 0.044, true, 0.686, 0.248, VerdictDifferentText, false},

		// No embedding row yet: an honest unknown, never a guess.
		{"anchor-low with no embedding row", 0.044, false, 0, 0, VerdictUnknown, false},

		// Near-boundary flagging on the anchor gate.
		{"hypothetical 0.86 anchor quality", 0.86, false, 0, 0, VerdictSameEdition, true},
	}
	for _, c := range cases {
		v := computeEditionVerdict(c.anchorQ, c.hasEmb, c.embq, c.share)
		if v.Bucket != c.wantBucket {
			t.Errorf("%s: bucket = %s, want %s (anchorQ=%.3f embq=%.3f share=%.3f)",
				c.name, v.Bucket, c.wantBucket, c.anchorQ, c.embq, c.share)
		}
		if v.NearBoundary != c.wantNearity {
			t.Errorf("%s: near_boundary = %v, want %v", c.name, v.NearBoundary, c.wantNearity)
		}
		if v.AnchorQuality != c.anchorQ {
			t.Errorf("%s: raw anchor quality not carried through", c.name)
		}
		if c.hasEmb && (v.EmbeddingSimilarity != c.embq || v.MatchedNarrationShare != c.share) {
			t.Errorf("%s: raw embedding numbers not carried through", c.name)
		}
	}
}

// Gulag's Detail must say the abridged-edition variant, not the generic one —
// the advice to a user differs ("expect gaps" vs "expect approximate").
func TestEditionVerdictAbridgedDetail(t *testing.T) {
	v := computeEditionVerdict(0.257, true, 0.831, 0.286)
	if v.Bucket != VerdictDifferentEdition {
		t.Fatalf("bucket = %s", v.Bucket)
	}
	generic := computeEditionVerdict(0.049, true, 0.774, 0.930)
	if v.Detail == generic.Detail {
		t.Errorf("abridged-edition case must carry a distinct detail from the plain different-edition case")
	}
}
