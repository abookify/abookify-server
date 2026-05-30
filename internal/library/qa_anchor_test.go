package library

import (
	"math"
	"testing"

	"github.com/pj/abookify/internal/db"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// TestAnchorTimesForWordPath verifies that a chunk's chapter-local word range
// is mapped to global offsets via EbookChapters and the audio times are read
// straight from the segment's baked WordSecs.
func TestAnchorTimesForWordPath(t *testing.T) {
	// Chapter 2 begins at global ebook word 100 and is 1000 words long.
	// One aligned segment covers [120, 130) of the ebook stream with per-word
	// start seconds 5.0..5.9, segment EndSec = 6.0.
	ws := []float64{5.0, 5.1, 5.2, 5.3, 5.4, 5.5, 5.6, 5.7, 5.8, 5.9}
	p := &AnchorAlignmentPayload{
		Method:        "anchor",
		Unit:          "word",
		EbookChapters: []ChapterSpan{{Index: 2, Start: 100, Len: 1000}},
		TransChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 5000}},
		Segments: []Segment{
			{EbookStart: 120, EbookEnd: 130, TransStart: 50, TransEnd: 60,
				Kind: SegAligned, StartSec: 5.0, EndSec: 6.0, WordSecs: ws},
		},
	}
	ac := &alignmentContext{audioBookID: 42}

	// Chunk covers ebook chapter-2 local words [22, 27). Global [122, 127).
	chunk := db.Chunk{BookID: 7, ChapterIdx: 2, StartWord: 22, EndWord: 27}
	bid, start, end, ok := ac.anchorTimesFor(chunk, p)
	if !ok {
		t.Fatalf("ok=false; want true")
	}
	if bid != 42 {
		t.Errorf("audioBookID=%d; want 42", bid)
	}
	if !approx(start, 5.2) { // WordSecs[122-120]
		t.Errorf("startSec=%v; want ~5.2", start)
	}
	if !approx(end, 5.7) { // WordSecs[127-120]
		t.Errorf("endSec=%v; want ~5.7", end)
	}
}

// TestAnchorTimesForWordPath_HalfOpenAtEnd: if the chunk's hi reaches the end
// of WordSecs, fall back to the segment's EndSec.
func TestAnchorTimesForWordPath_HalfOpenAtEnd(t *testing.T) {
	ws := []float64{5.0, 5.1, 5.2, 5.3}
	p := &AnchorAlignmentPayload{
		EbookChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 100}},
		Segments: []Segment{
			{EbookStart: 0, EbookEnd: 4, TransStart: 0, TransEnd: 4,
				Kind: SegAligned, StartSec: 5.0, EndSec: 6.0, WordSecs: ws},
		},
	}
	ac := &alignmentContext{audioBookID: 1}
	chunk := db.Chunk{ChapterIdx: 0, StartWord: 2, EndWord: 4} // hi==len(ws)
	_, _, end, ok := ac.anchorTimesFor(chunk, p)
	if !ok {
		t.Fatal("ok=false")
	}
	if !approx(end, 6.0) {
		t.Errorf("end=%v; want EndSec 6.0", end)
	}
}

// TestAnchorTimesForParagraphPath verifies the WordSecs-empty branch
// interpolates within the segment's [StartSec, EndSec] range.
func TestAnchorTimesForParagraphPath(t *testing.T) {
	p := &AnchorAlignmentPayload{
		Method:        "embedding",
		Unit:          "paragraph",
		EbookChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 1000}},
		Segments: []Segment{
			// 100-word segment, 10 seconds of audio.
			{EbookStart: 0, EbookEnd: 100, TransStart: 0, TransEnd: 80,
				Kind: SegAligned, StartSec: 10.0, EndSec: 20.0},
		},
	}
	ac := &alignmentContext{audioBookID: 5}
	// Middle 25-word slice: words [25, 50). Expect 12.5 .. 15.0.
	chunk := db.Chunk{ChapterIdx: 0, StartWord: 25, EndWord: 50}
	_, start, end, ok := ac.anchorTimesFor(chunk, p)
	if !ok {
		t.Fatal("ok=false")
	}
	if !approx(start, 12.5) {
		t.Errorf("start=%v; want 12.5", start)
	}
	if !approx(end, 15.0) {
		t.Errorf("end=%v; want 15.0", end)
	}
}

// TestAnchorTimesForDivergent verifies a chunk entirely within an ebook-only
// (skipped) segment returns ok=false.
func TestAnchorTimesForDivergent(t *testing.T) {
	p := &AnchorAlignmentPayload{
		EbookChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 1000}},
		Segments: []Segment{
			{EbookStart: 0, EbookEnd: 50, TransStart: 0, TransEnd: 50, Kind: SegAligned,
				StartSec: 0, EndSec: 5, WordSecs: make([]float64, 50)},
			// Ebook-only gap: words 50..100 are in the book but not narrated.
			{EbookStart: 50, EbookEnd: 100, TransStart: 50, TransEnd: 50, Kind: SegEbookOnly},
		},
	}
	ac := &alignmentContext{audioBookID: 1}
	chunk := db.Chunk{ChapterIdx: 0, StartWord: 60, EndWord: 80}
	_, _, _, ok := ac.anchorTimesFor(chunk, p)
	if ok {
		t.Error("ok=true for chunk in ebook-only segment; want false")
	}
}

// TestAnchorTimesForUnknownChapter verifies a chunk whose ebook chapter is
// missing from EbookChapters (e.g. boilerplate stripped before alignment)
// returns ok=false.
func TestAnchorTimesForUnknownChapter(t *testing.T) {
	p := &AnchorAlignmentPayload{
		EbookChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 100}},
	}
	ac := &alignmentContext{audioBookID: 1}
	chunk := db.Chunk{ChapterIdx: 7, StartWord: 0, EndWord: 5}
	if _, _, _, ok := ac.anchorTimesFor(chunk, p); ok {
		t.Error("ok=true for unknown chapter; want false")
	}
}
