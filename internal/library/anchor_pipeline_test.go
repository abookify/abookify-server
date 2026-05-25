package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestIsBoilerplateChapterTitle(t *testing.T) {
	boiler := []string{
		"CONTENTS", "Contents", "  Index ", "End Notes", "Notes",
		"A Note on the Type", "Acknowledgments", "About the Author",
		"The Project Gutenberg eBook of Frankenstein; or, the modern prometheus",
		"END OF THE PROJECT GUTENBERG EBOOK", "Illustration Permissions",
		"Copyright", "Title Page", "Index",
	}
	for _, s := range boiler {
		if !IsBoilerplateChapterTitle(s) {
			t.Errorf("expected boilerplate: %q", s)
		}
	}
	content := []string{
		"Chapter 5", "FOOD IS GOOD", "Letter 1", "Vol. II, Chapter IX",
		"Notes from Underground", "The Republic", "Prelude", "Book I",
		"", "  ",
	}
	for _, s := range content {
		if IsBoilerplateChapterTitle(s) {
			t.Errorf("expected content (not boilerplate): %q", s)
		}
	}
}

func TestAssembleStream_AndLocateGlobal(t *testing.T) {
	chs := []ChapterText{
		{Index: 2, Text: "alpha bravo charlie"},     // 3 words
		{Index: 3, Text: ""},                        // empty -> skipped, no span
		{Index: 4, Text: "delta echo foxtrot golf"}, // 4 words
	}
	toks, spans := AssembleStream(chs)
	if len(toks) != 7 {
		t.Fatalf("want 7 tokens, got %d", len(toks))
	}
	if len(spans) != 2 {
		t.Fatalf("want 2 spans (empty chapter skipped), got %d", len(spans))
	}
	if spans[0] != (ChapterSpan{Index: 2, Start: 0, Len: 3}) {
		t.Errorf("span0 = %+v", spans[0])
	}
	if spans[1] != (ChapterSpan{Index: 4, Start: 3, Len: 4}) {
		t.Errorf("span1 = %+v", spans[1])
	}
	// global offset 0 -> chapter 2, local 0
	if ci, lp, ok := LocateGlobal(spans, 0); !ok || ci != 2 || lp != 0 {
		t.Errorf("LocateGlobal(0) = (%d,%d,%v)", ci, lp, ok)
	}
	// global offset 4 -> chapter 4, local 1 ("echo")
	if ci, lp, ok := LocateGlobal(spans, 4); !ok || ci != 4 || lp != 1 {
		t.Errorf("LocateGlobal(4) = (%d,%d,%v)", ci, lp, ok)
	}
	// past the end -> not ok
	if _, _, ok := LocateGlobal(spans, 7); ok {
		t.Errorf("LocateGlobal(7) should be out of range")
	}
}

func TestMapEbookToTrans(t *testing.T) {
	payload := AnchorAlignmentPayload{
		Segments: []Segment{
			{EbookStart: 0, EbookEnd: 100, TransStart: 0, TransEnd: 100, Kind: SegAligned},
			{EbookStart: 100, EbookEnd: 150, TransStart: 100, TransEnd: 100, Kind: SegEbookOnly}, // gap
			{EbookStart: 150, EbookEnd: 250, TransStart: 100, TransEnd: 200, Kind: SegAligned},
		},
	}
	// A range inside the first aligned segment maps ~1:1.
	ts, te, ok := MapEbookToTrans(payload, 10, 20)
	if !ok || ts != 10 || te != 20 {
		t.Errorf("range [10,20) -> (%d,%d,%v), want (10,20,true)", ts, te, ok)
	}
	// A range entirely inside the ebook-only gap maps nowhere.
	if _, _, ok := MapEbookToTrans(payload, 110, 140); ok {
		t.Errorf("range in ebook-only gap should not map")
	}
	// A range in the second aligned segment (offset by the gap) maps correctly.
	ts, te, ok = MapEbookToTrans(payload, 150, 200)
	if !ok || ts != 100 || te != 150 {
		t.Errorf("range [150,200) -> (%d,%d,%v), want (100,150,true)", ts, te, ok)
	}
}

func TestSummarizeAnchorDivergence(t *testing.T) {
	segs := []Segment{
		{EbookStart: 0, EbookEnd: 100, TransStart: 0, TransEnd: 100, Kind: SegAligned},
		{EbookStart: 100, EbookEnd: 100, TransStart: 100, TransEnd: 130, Kind: SegTransOnly}, // +30 trans
		{EbookStart: 100, EbookEnd: 5100, TransStart: 130, TransEnd: 130, Kind: SegEbookOnly}, // +5000 ebook (biggest)
		{EbookStart: 5100, EbookEnd: 5110, TransStart: 130, TransEnd: 145, Kind: SegReplace},  // +10/+15
	}
	d := summarizeAnchorDivergence(segs)
	if d.AlignedSegs != 1 || d.EbookOnlySegs != 1 || d.TransOnlySegs != 1 || d.ReplaceSegs != 1 {
		t.Errorf("seg counts wrong: %+v", d)
	}
	if d.EbookOnlyWords != 5000+10 {
		t.Errorf("ebook-only words = %d, want 5010", d.EbookOnlyWords)
	}
	if d.TransOnlyWords != 30+15 {
		t.Errorf("trans-only words = %d, want 45", d.TransOnlyWords)
	}
	// Biggest divergence (the 5000-word ebook-only block) is first in Top.
	if len(d.Top) == 0 || d.Top[0].Kind != SegEbookOnly {
		t.Errorf("Top[0] should be the 5000-word ebook-only segment, got %+v", d.Top)
	}
}

func TestBuildTokToFields(t *testing.T) {
	// "well-known" is 1 Fields word but 2 Tokenize tokens (well, known);
	// "don't" is 1 of each. So Tokenize stream [well,known,don't,fox] maps to
	// Fields indices [0,0,1,2].
	chs := []ChapterText{{Index: 0, Text: "well-known don't fox"}}
	m := buildTokToFields(chs)
	want := []int{0, 0, 1, 2}
	if len(m) != len(want) {
		t.Fatalf("len=%d want %d (%v)", len(m), len(want), m)
	}
	for i := range want {
		if m[i] != want[i] {
			t.Errorf("tokToFields[%d]=%d want %d", i, m[i], want[i])
		}
	}
}

func TestBakeSegmentTimes_WordPath(t *testing.T) {
	// timeline: 4 transcript words at 1s apart.
	tl := []db.SyncTimestamp{{Start: 0, End: 0.9}, {Start: 1, End: 1.9}, {Start: 2, End: 2.9}, {Start: 3, End: 3.9}}
	segs := []Segment{
		{EbookStart: 0, EbookEnd: 4, TransStart: 0, TransEnd: 4, Kind: SegAligned},
		{EbookStart: 4, EbookEnd: 6, TransStart: 4, TransEnd: 4, Kind: SegEbookOnly},
	}
	bakeSegmentTimes(segs, tl, nil, true) // identity basis (Fields), word path
	a := segs[0]
	if a.StartSec != 0 || a.EndSec != 3.9 {
		t.Errorf("aligned ss/se = %v/%v, want 0/3.9", a.StartSec, a.EndSec)
	}
	if len(a.WordSecs) != 4 || a.WordSecs[0] != 0 || a.WordSecs[3] != 3 {
		t.Errorf("WordSecs = %v, want [0 1 2 3]", a.WordSecs)
	}
	// divergence segment carries no times
	if segs[1].StartSec != 0 || segs[1].WordSecs != nil {
		t.Errorf("ebook-only segment should have no baked times: %+v", segs[1])
	}
}

func TestBakeSegmentTimes_TokenizeBasis(t *testing.T) {
	// Transcript Tokenize stream of 4 tokens maps to 3 sync words via
	// tokToFields [0,0,1,2] (e.g. "well-known don't fox"). A segment over
	// Tokenize [0,4) should resolve start=word0, end=word2.
	tl := []db.SyncTimestamp{{Start: 10, End: 10.5}, {Start: 11, End: 11.5}, {Start: 12, End: 12.5}}
	tok2f := []int{0, 0, 1, 2}
	segs := []Segment{{EbookStart: 0, EbookEnd: 4, TransStart: 0, TransEnd: 4, Kind: SegAligned}}
	bakeSegmentTimes(segs, tl, tok2f, false)
	if segs[0].StartSec != 10 || segs[0].EndSec != 12.5 {
		t.Errorf("ss/se = %v/%v, want 10/12.5", segs[0].StartSec, segs[0].EndSec)
	}
}
