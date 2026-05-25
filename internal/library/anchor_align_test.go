package library

import (
	"strings"
	"testing"
)

// toks is a tiny helper: whitespace-split (inputs here are already lowercase
// and punctuation-free so we exercise the alignment logic, not Tokenize).
func toks(s string) []string { return strings.Fields(s) }

// A reusable body of "prose" long enough that 4-grams are unique. Distinct
// words keep every 4-gram hapax, which is the regime the real data sits in
// (~80% of 4-grams unique).
const prose = "alpha bravo charlie delta echo foxtrot golf hotel india juliet " +
	"kilo lima mike november oscar papa quebec romeo sierra tango " +
	"uniform victor whiskey xray yankee zulu one two three four"

func TestTokenize_NormalizesPunctAndCase(t *testing.T) {
	got := Tokenize("  The QUICK—brown, don't fox! 42 ")
	want := []string{"the", "quick", "brown", "don't", "fox", "42"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Tokenize = %v, want %v", got, want)
	}
}

func TestAlign_Identical_FullCoverageNoDivergence(t *testing.T) {
	w := toks(prose)
	a := Align(w, w, 4)
	if cov := a.Coverage(len(w)); cov < 0.999 {
		t.Errorf("identical sequences should be ~100%% covered, got %.3f", cov)
	}
	for _, s := range a.Segments {
		if s.Kind != SegAligned {
			t.Errorf("identical sequences should have no divergence, got %s [%d:%d]", s.Kind, s.EbookStart, s.EbookEnd)
		}
		// On identical input the matched run maps a position to itself.
		if s.EbookStart != s.TransStart || s.EbookEnd != s.TransEnd {
			t.Errorf("identical: ebook span [%d:%d] != trans span [%d:%d]", s.EbookStart, s.EbookEnd, s.TransStart, s.TransEnd)
		}
	}
}

func TestAlign_TranscriptIntro_IsTransOnlyAtHead(t *testing.T) {
	// Narrator preamble the ebook doesn't have ("this is a librivox recording").
	ebook := toks(prose)
	trans := toks("this is a librivox recording all rights reversed " + prose)
	a := Align(ebook, trans, 4)

	if len(a.Segments) == 0 {
		t.Fatal("no segments")
	}
	head := a.Segments[0]
	if head.Kind != SegTransOnly {
		t.Fatalf("expected leading trans-only divergence (the intro), got %s", head.Kind)
	}
	if head.TransStart != 0 || head.TransEnd != 8 {
		t.Errorf("intro should be transcript [0:8], got [%d:%d]", head.TransStart, head.TransEnd)
	}
	// Ebook is still fully covered despite the intro.
	if cov := a.Coverage(len(ebook)); cov < 0.999 {
		t.Errorf("ebook should be fully covered, got %.3f", cov)
	}
}

func TestAlign_AudioSkipsParagraph_IsEbookOnlyDivergence(t *testing.T) {
	// Ebook has a middle block the audio (transcript) skipped — abridgment.
	head := "alpha bravo charlie delta echo foxtrot golf hotel"
	skipped := "secret skipped passage that never got narrated aloud here"
	tail := "india juliet kilo lima mike november oscar papa quebec romeo"
	ebook := toks(head + " " + skipped + " " + tail)
	trans := toks(head + " " + tail)
	a := Align(ebook, trans, 4)

	var sawEbookOnly bool
	for _, s := range a.Segments {
		if s.Kind == SegEbookOnly {
			sawEbookOnly = true
			got := strings.Join(ebook[s.EbookStart:s.EbookEnd], " ")
			if !strings.Contains(got, "skipped passage") {
				t.Errorf("ebook-only span should contain the skipped text, got %q", got)
			}
		}
	}
	if !sawEbookOnly {
		t.Error("expected an ebook-only divergence segment for the skipped paragraph")
	}
}

func TestAlign_SingleSTTError_BridgedBySurroundingAnchors(t *testing.T) {
	// One word mistranscribed in the middle. 4-grams not touching it still
	// anchor on both sides, so the alignment spans the whole thing and the
	// bad word lands in a small replace gap rather than derailing.
	ebook := toks(prose)
	trWords := toks(prose)
	trWords[15] = "MISHEARD" // corrupt one word
	a := Align(ebook, trWords, 4)

	if cov := a.Coverage(len(ebook)); cov < 0.70 {
		t.Errorf("a single STT error should leave most of the ebook covered, got %.3f", cov)
	}
	// The corrupted position should sit inside a replace gap (both sides have
	// content there) — not split the book into two unrelated halves.
	if len(a.Anchors) < 10 {
		t.Errorf("expected anchors on both sides of the error, got %d", len(a.Anchors))
	}
}

func TestMonotonicChain_RejectsOutOfOrderMatch(t *testing.T) {
	// A phrase that appears once in the ebook but twice in the transcript:
	// once in the correct (monotonic) place and once earlier (out of order).
	// The chain must keep the consistent one.
	cands := []Anchor{
		{EbookPos: 10, TransPos: 12}, // consistent
		{EbookPos: 20, TransPos: 3},  // out of order (would break monotonicity)
		{EbookPos: 20, TransPos: 25}, // consistent alternative for the repeated phrase
		{EbookPos: 30, TransPos: 40}, // consistent
	}
	chain := MonotonicChain(cands)
	// Expect the strictly-increasing chain 12 < 25 < 40 (length 3), dropping
	// the out-of-order TransPos=3.
	if len(chain) != 3 {
		t.Fatalf("want chain length 3, got %d: %+v", len(chain), chain)
	}
	last := -1
	for _, an := range chain {
		if an.TransPos <= last {
			t.Errorf("chain not strictly increasing in TransPos: %+v", chain)
		}
		last = an.TransPos
	}
}

func TestMonotonicChain_AtMostOneAnchorPerEbookPos(t *testing.T) {
	// Same ebook position offered with several transcript matches — only one
	// may appear in the final chain.
	cands := []Anchor{
		{EbookPos: 5, TransPos: 7},
		{EbookPos: 5, TransPos: 9},
		{EbookPos: 5, TransPos: 11},
	}
	chain := MonotonicChain(cands)
	if len(chain) != 1 {
		t.Fatalf("a single ebook position must yield at most one anchor, got %d", len(chain))
	}
}

func TestAlign_TrailingBoilerplate_IsEbookOnly(t *testing.T) {
	// The Project Gutenberg license case: a large block at the end of the
	// ebook with no audio counterpart.
	body := prose
	license := "end of the project gutenberg ebook this work is in the public domain " +
		"redistribution is permitted subject to the gutenberg license terms herein"
	ebook := toks(body + " " + license)
	trans := toks(body)
	a := Align(ebook, trans, 4)

	if len(a.Segments) == 0 {
		t.Fatal("no segments")
	}
	tailSeg := a.Segments[len(a.Segments)-1]
	if tailSeg.Kind != SegEbookOnly {
		t.Fatalf("trailing license should be ebook-only divergence, got %s", tailSeg.Kind)
	}
	if tailSeg.EbookEnd != len(ebook) {
		t.Errorf("trailing divergence should run to end of ebook (%d), got %d", len(ebook), tailSeg.EbookEnd)
	}
}

func TestFindAnchors_SkipsAmbiguousEbookNgrams(t *testing.T) {
	// "of the" repeated in the ebook → its n-grams are not hapax → not anchors.
	ebook := toks("one of the things and many of the other items here now")
	trans := toks("one of the things and many of the other items here now")
	anchors := FindAnchors(ebook, trans, 2)
	for _, an := range anchors {
		g := strings.Join(ebook[an.EbookPos:an.EbookPos+2], " ")
		if g == "of the" {
			t.Errorf("'of the' occurs twice in ebook and must not be an anchor")
		}
	}
}
