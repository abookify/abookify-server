package library

import (
	"strings"
	"testing"
)

func TestSplitIntoParagraphs_EPUBStyle(t *testing.T) {
	// EPUB-style input: one paragraph per line, block-level tags converted to \n.
	content := "It was the best of times.\nIt was the worst of times.\n\nThe age of wisdom.\nThe age of foolishness."
	paras := SplitIntoParagraphs(1, 0, content)
	if len(paras) != 4 {
		t.Fatalf("want 4 paragraphs, got %d: %+v", len(paras), paras)
	}
	// Word cursor accumulates across paragraphs ("It was the best of times." = 6 words).
	if paras[0].WordStart != 0 || paras[0].WordEnd != 6 {
		t.Errorf("p0 range: [%d, %d), want [0, 6)", paras[0].WordStart, paras[0].WordEnd)
	}
	if paras[1].WordStart != 6 {
		t.Errorf("p1 start: %d, want 6", paras[1].WordStart)
	}
	// Indices assigned in order from 0.
	for i, p := range paras {
		if p.ParagraphIdx != i {
			t.Errorf("paragraph %d has idx %d", i, p.ParagraphIdx)
		}
	}
}

func TestSplitIntoParagraphs_TranscriptFallback(t *testing.T) {
	// Transcript-style: one long run with no newlines, ~200 words.
	// Should trigger sentence-based fallback and produce ~3-4 paragraphs.
	content := strings.Repeat("This is a sentence. Another follows immediately. Then another one comes. ", 15)
	paras := SplitIntoParagraphs(1, 0, content)
	if len(paras) < 2 {
		t.Fatalf("expected ≥2 paragraphs from fallback, got %d", len(paras))
	}
	// Each paragraph roughly targets 60 words.
	for _, p := range paras[:len(paras)-1] { // last can be short
		w := p.WordEnd - p.WordStart
		if w > paragraphTargetWords*2 {
			t.Errorf("paragraph %d much larger than target: %d words", p.ParagraphIdx, w)
		}
	}
	// Word offsets are monotonic and contiguous.
	prev := 0
	for _, p := range paras {
		if p.WordStart != prev {
			t.Errorf("p%d WordStart=%d, want %d (contiguous)", p.ParagraphIdx, p.WordStart, prev)
		}
		prev = p.WordEnd
	}
}

func TestSplitIntoParagraphs_Empty(t *testing.T) {
	if got := SplitIntoParagraphs(1, 0, ""); got != nil {
		t.Errorf("empty → nil; got %+v", got)
	}
	if got := SplitIntoParagraphs(1, 0, "   \n  \n  "); len(got) != 0 {
		t.Errorf("whitespace-only → 0 paragraphs; got %d", len(got))
	}
}

func TestSplitIntoParagraphs_ShortSingleLine(t *testing.T) {
	// One short line (under the fallback threshold) stays as a single paragraph.
	content := "Hello world."
	paras := SplitIntoParagraphs(1, 0, content)
	if len(paras) != 1 {
		t.Fatalf("want 1 paragraph, got %d", len(paras))
	}
	if paras[0].WordEnd != 2 {
		t.Errorf("word count: %d, want 2", paras[0].WordEnd)
	}
}
