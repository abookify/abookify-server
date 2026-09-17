package library

import "testing"

// The Selfish Gene shape: a clean chain interrupted by inline endnotes the EPUB
// omits. Whole-book audio_to_ebook understates the chain; the in-text figure
// excludes the narrated-extra runs and reads the chain where the ebook has text.
func TestDirectionalFrom_NarratedExtraExcluded(t *testing.T) {
	p := AnchorAlignmentPayload{
		EbookWords: 1030,
		TransWords: 2200,
		Segments: []Segment{
			{Kind: SegAligned, EbookStart: 0, EbookEnd: 500, TransStart: 0, TransEnd: 500},
			{Kind: SegTransOnly, EbookStart: 500, EbookEnd: 500, TransStart: 500, TransEnd: 900}, // endnote: 400 narrated, nothing in the ebook
			{Kind: SegAligned, EbookStart: 500, EbookEnd: 800, TransStart: 900, TransEnd: 1200},
			{Kind: SegReplace, EbookStart: 800, EbookEnd: 820, TransStart: 1200, TransEnd: 1700},     // endnote with 20 ebook words caught between anchors
			{Kind: SegReplace, EbookStart: 820, EbookEnd: 1000, TransStart: 1700, TransEnd: 2000},    // real divergence: text on both sides
			{Kind: SegTransOnly, EbookStart: 1000, EbookEnd: 1000, TransStart: 2000, TransEnd: 2100}, // short ad-lib: under the run size, still counts against
			{Kind: SegAligned, EbookStart: 1000, EbookEnd: 1030, TransStart: 2100, TransEnd: 2200},
		},
	}
	p.Divergence.TransOnlyWords = 400 + 500 + 300 + 100 // 1300 unaligned narration
	p.Divergence.EbookOnlyWords = 20 + 180
	d := directionalFrom(p, 0, 0)
	if d.NarratedExtraWords != 900 {
		t.Fatalf("narrated extra = %d, want 900 (400 endnote + 500 asymmetric replace)", d.NarratedExtraWords)
	}
	wantWhole := float64(2200-1300) / 2200
	if d.AudioToEbook < wantWhole-1e-9 || d.AudioToEbook > wantWhole+1e-9 {
		t.Fatalf("audio_to_ebook = %.3f, want %.3f (unchanged whole-book figure)", d.AudioToEbook, wantWhole)
	}
	wantInText := float64(2200-1300) / float64(2200-900)
	if d.AudioToEbookInText < wantInText-1e-9 || d.AudioToEbookInText > wantInText+1e-9 {
		t.Fatalf("audio_to_ebook_in_text = %.3f, want %.3f", d.AudioToEbookInText, wantInText)
	}
	if d.AudioToEbookInText <= d.AudioToEbook {
		t.Fatal("in-text quality must exceed the whole-book figure when narrated extra exists")
	}
}

// A weak chain — unaligned narration scattered in short runs and in replace runs
// with real text on both sides — must read the same on both figures.
func TestDirectionalFrom_WeakChainNotHidden(t *testing.T) {
	var segs []Segment
	e, tr := 0, 0
	for i := 0; i < 20; i++ {
		segs = append(segs, Segment{Kind: SegAligned, EbookStart: e, EbookEnd: e + 50, TransStart: tr, TransEnd: tr + 50})
		e += 50
		tr += 50
		segs = append(segs, Segment{Kind: SegReplace, EbookStart: e, EbookEnd: e + 400, TransStart: tr, TransEnd: tr + 450})
		e += 400
		tr += 450
	}
	p := AnchorAlignmentPayload{EbookWords: e, TransWords: tr, Segments: segs}
	p.Divergence.TransOnlyWords = 20 * 450
	p.Divergence.EbookOnlyWords = 20 * 400
	d := directionalFrom(p, 0, 0)
	if d.NarratedExtraWords != 0 {
		t.Fatalf("narrated extra = %d on a weak chain, want 0", d.NarratedExtraWords)
	}
	if d.AudioToEbookInText != d.AudioToEbook {
		t.Fatalf("in-text %.3f != whole-book %.3f on a weak chain", d.AudioToEbookInText, d.AudioToEbook)
	}
	if d.AudioToEbookInText >= minChainConfidence {
		t.Fatalf("weak chain %.3f passed the reader gate", d.AudioToEbookInText)
	}
}
