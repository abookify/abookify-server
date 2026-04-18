package library

import (
	"strings"
	"testing"
)

func norm(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = normalizeWord(w)
	}
	return out
}

func TestAlignWordsDP_ExactMatch(t *testing.T) {
	words := strings.Fields("the quick brown fox jumps over the lazy dog")
	matches := alignWordsDP(norm(words), norm(words))
	if len(matches) != 9 {
		t.Fatalf("exact match: want 9 pairs, got %d", len(matches))
	}
	for _, m := range matches {
		if m.ebookIdx != m.transcriptIdx {
			t.Errorf("exact match should be 1:1, got ebook=%d transcript=%d", m.ebookIdx, m.transcriptIdx)
		}
	}
	conf := alignmentConfidence(matches, norm(words), norm(words))
	if conf != 1.0 {
		t.Errorf("exact match confidence should be 1.0, got %f", conf)
	}
}

func TestAlignWordsDP_Substitution(t *testing.T) {
	ebook := norm(strings.Fields("the quick brown fox"))
	// Whisper misheard "brown" as "round"
	transcript := norm(strings.Fields("the quick round fox"))
	matches := alignWordsDP(ebook, transcript)

	// All 4 words should have a match (even though "brown"↔"round" is a mismatch).
	ebookMatched := 0
	for _, m := range matches {
		if m.ebookIdx >= 0 && m.transcriptIdx >= 0 {
			ebookMatched++
		}
	}
	if ebookMatched != 4 {
		t.Errorf("want 4 paired words, got %d", ebookMatched)
	}
	conf := alignmentConfidence(matches, ebook, transcript)
	if conf < 0.7 || conf > 0.8 {
		t.Errorf("confidence with 1 mismatch of 4: expected ~0.75, got %f", conf)
	}
}

func TestAlignWordsDP_Insertion(t *testing.T) {
	ebook := norm(strings.Fields("the quick brown fox"))
	// Whisper hallucinated "very" between "quick" and "brown"
	transcript := norm(strings.Fields("the quick very brown fox"))
	matches := alignWordsDP(ebook, transcript)

	// Should align: the↔the, quick↔quick, brown↔brown, fox↔fox
	// with "very" in transcript gapped.
	conf := alignmentConfidence(matches, ebook, transcript)
	if conf != 1.0 {
		t.Errorf("all ebook words should match despite insertion: conf=%f", conf)
	}
}

func TestAlignWordsDP_Deletion(t *testing.T) {
	ebook := norm(strings.Fields("the quick brown fox jumps"))
	// Whisper missed "brown"
	transcript := norm(strings.Fields("the quick fox jumps"))
	matches := alignWordsDP(ebook, transcript)

	// "brown" in ebook should be gapped (no transcript match).
	conf := alignmentConfidence(matches, ebook, transcript)
	if conf < 0.7 || conf > 0.85 {
		t.Errorf("1 deletion out of 5: expected conf ~0.8, got %f", conf)
	}
}

func TestAlignWordsDP_LargerText(t *testing.T) {
	// Simulate a ~100-word paragraph with a few differences.
	base := "In the beginning God created the heavens and the earth. " +
		"Now the earth was formless and empty, darkness was over the surface of the deep. " +
		"And the Spirit of God was hovering over the waters. " +
		"And God said, Let there be light, and there was light."
	ebook := norm(strings.Fields(base))
	// Transcript: few misheards
	transcript := norm(strings.Fields(
		"In the beginning God created the heavens and the earth. " +
			"Now the earth was formless and empty, darkness was over the surface of the deep. " +
			"And the spirit of God was hovering over the waters. " +
			"And God said, let there be light, and there was light."))

	matches := alignWordsDP(ebook, transcript)
	conf := alignmentConfidence(matches, ebook, transcript)
	if conf < 0.95 {
		t.Errorf("mostly identical text: expected high conf, got %f", conf)
	}
}

func TestAlignWordsDP_Empty(t *testing.T) {
	if m := alignWordsDP(nil, norm(strings.Fields("hello"))); m != nil {
		t.Error("empty ebook should return nil")
	}
	if m := alignWordsDP(norm(strings.Fields("hello")), nil); m != nil {
		t.Error("empty transcript should return nil")
	}
}

func TestTranscriptRangeForEbookRange(t *testing.T) {
	matches := []wordMatch{
		{0, 0}, {1, 1}, {2, 2}, {3, -1}, {4, 3}, {5, 4}, {6, 5},
	}
	// Ebook words 2-5 should map to transcript 2-4.
	start, end := transcriptRangeForEbookRange(matches, 2, 5)
	if start != 2 || end != 4 {
		t.Errorf("range [2,5) → transcript [%d,%d), want [2,4)", start, end)
	}
}
