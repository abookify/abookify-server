package library

import "testing"

// normalizeForCompare decides whether a "replace" span is a real substitution
// or a presentational artifact. These cases (from PJ's meld testing) must
// normalize-equal so they're NOT flagged as divergences; genuine word
// substitutions must remain distinct.
func TestNormalizeForCompare(t *testing.T) {
	equal := [][2]string{
		{"You", "you"},                         // case
		{`he said "You`, "he said You"},         // STT leading-quote artifact
		{"stomped on to", "stomped onto"},       // compounding + whitespace
		{"hung over", "hungover"},               // compounding
		{"Don't,", "dont"},                      // apostrophe + comma
		{"well-being", "well being"},            // hyphen vs space
		{"“Hello,” she said.", "hello she said"}, // smart quotes + punctuation
	}
	for _, c := range equal {
		if normalizeForCompare(c[0]) != normalizeForCompare(c[1]) {
			t.Errorf("should be equal after normalize: %q vs %q → %q vs %q",
				c[0], c[1], normalizeForCompare(c[0]), normalizeForCompare(c[1]))
		}
	}
	differ := [][2]string{
		{"stomped onto", "stamped onto"}, // real substitution
		{"he said you", "she said you"},  // real substitution
		{"the cat sat", "the cat"},       // deletion
	}
	for _, c := range differ {
		if normalizeForCompare(c[0]) == normalizeForCompare(c[1]) {
			t.Errorf("should DIFFER after normalize (genuine change): %q vs %q", c[0], c[1])
		}
	}
}

// The diff text recovery (BuildDiff) maps alignment word-offsets — computed
// against the Tokenize stream — onto a case-preserving displayTokenize stream.
// That only yields faithful span text if the two tokenizers produce the SAME
// number of tokens at the SAME positions for every input. This locks that
// invariant: if someone edits either regex/normalizer and the counts drift,
// span text would silently misalign on the mobile-facing /diff endpoint.
func TestDisplayTokenizeMatchesTokenize(t *testing.T) {
	cases := []string{
		"",
		"plain lowercase words",
		"Title Case With Caps",
		"ALL CAPS HEADING",
		"don't can't it's o'clock",          // apostrophes
		"Chapter 1: the year 1984, vol. 2",  // digits + punctuation
		"em—dash and ‘curly’ and “quotes”",  // unicode punctuation
		"naïve café résumé Zoë",             // accented letters (stripped both ways)
		"Mr. Frankenstein, who had spoken,", // commas/periods
		"line one\nline two\ttabbed   spaced",
		"trailing punctuation!!! ??? ...",
		"Hyphen-ated and slash/separated words",
		"It was a bright cold day in April, and the clocks were striking thirteen.",
	}
	for _, in := range cases {
		canonical := Tokenize(in)
		display := displayTokenize(in)
		if len(canonical) != len(display) {
			t.Errorf("token count mismatch for %q: Tokenize=%d displayTokenize=%d\n  canonical=%v\n  display=%v",
				in, len(canonical), len(display), canonical, display)
			continue
		}
		// Each display token must lowercase to its canonical counterpart, so
		// positions truly correspond (not just counts).
		for i := range canonical {
			if lower(display[i]) != canonical[i] {
				t.Errorf("token %d mismatch for %q: canonical=%q display=%q",
					i, in, canonical[i], display[i])
			}
		}
	}
}

// Directional coverage (#199) must read BOTH ways out of one payload: scope
// (ebook→audio) = aligned_ebook/ebook, and quality (audio→ebook) =
// aligned_trans/trans, where aligned = words NOT in an {ebook,trans}-only
// segment. Locks the Heart of Darkness shape (the bug a single number caused):
// 33% scope but 92% quality.
func TestDirectionalFromHeartOfDarkness(t *testing.T) {
	p := AnchorAlignmentPayload{
		EbookWords: 109728,
		TransWords: 39822,
		Divergence: DivergenceSummary{
			EbookOnlyWords: 73246,
			TransOnlyWords: 3232,
		},
	}
	d := directionalFrom(p, 0, 0)
	if d.AlignedEbookWords != 36482 {
		t.Errorf("aligned_ebook_words = %d, want 36482", d.AlignedEbookWords)
	}
	if d.AlignedTransWords != 36590 {
		t.Errorf("aligned_trans_words = %d, want 36590", d.AlignedTransWords)
	}
	if got := round2(d.EbookToAudio); got != 0.33 {
		t.Errorf("ebook_to_audio (scope) = %.4f, want ~0.33", d.EbookToAudio)
	}
	if got := round2(d.AudioToEbook); got != 0.92 {
		t.Errorf("audio_to_ebook (quality) = %.4f, want ~0.92", d.AudioToEbook)
	}
}

// directionalFrom must not divide by zero or report negative aligned counts
// when a payload is empty or self-inconsistent (older/partial rows).
func TestDirectionalFromDegenerate(t *testing.T) {
	// Empty payload → all zero, no NaN/Inf.
	d := directionalFrom(AnchorAlignmentPayload{}, 0, 0)
	if d.AudioToEbook != 0 || d.EbookToAudio != 0 {
		t.Errorf("empty payload should give 0 ratios, got %+v", d)
	}
	// only_words exceeding totals must clamp aligned to 0, not go negative.
	d = directionalFrom(AnchorAlignmentPayload{
		EbookWords: 100, TransWords: 100,
		Divergence: DivergenceSummary{EbookOnlyWords: 250, TransOnlyWords: 250},
	}, 0, 0)
	if d.AlignedEbookWords != 0 || d.AlignedTransWords != 0 {
		t.Errorf("aligned counts should clamp to 0, got ebook=%d trans=%d", d.AlignedEbookWords, d.AlignedTransWords)
	}
	// Fallbacks fill missing word counts.
	d = directionalFrom(AnchorAlignmentPayload{}, 50, 80)
	if d.EbookWords != 50 || d.TransWords != 80 {
		t.Errorf("fallbacks not applied: ebook=%d trans=%d", d.EbookWords, d.TransWords)
	}
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

// lower mirrors Tokenize's only case transform (ToLower) for the position check.
func lower(s string) string {
	b := []rune(s)
	for i, r := range b {
		if r >= 'A' && r <= 'Z' {
			b[i] = r + 32
		}
	}
	return string(b)
}
