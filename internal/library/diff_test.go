package library

import "testing"

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
