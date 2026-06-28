package library

import "testing"

func TestBuildRenderTimeline_WordModeDense(t *testing.T) {
	// Two ebook chapters: [0,3) and [3,6). One aligned word-segment spanning
	// words 0..4 with per-word secs; words 4,5 unaligned.
	p := &AnchorAlignmentPayload{
		Unit:          "word",
		EbookChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 3}, {Index: 1, Start: 3, Len: 3}},
		Segments: []Segment{
			{EbookStart: 0, EbookEnd: 4, Kind: SegAligned, StartSec: 1.0, EndSec: 4.0,
				WordSecs: []float64{1.0, 2.0, 3.0, 4.0}},
		},
	}
	buildRenderTimeline(p)

	if len(p.Timeline) != 2 {
		t.Fatalf("want 2 chapter timelines, got %d", len(p.Timeline))
	}
	// Chapter 0: words 0,1,2 (chapter-relative 0,1,2) at 1,2,3s.
	c0 := p.Timeline[0]
	if c0.EbookChapterIdx != 0 || c0.Unit != "word" || len(c0.Points) != 3 {
		t.Fatalf("ch0 unexpected: %+v", c0)
	}
	if c0.Points[0] != (TimelinePoint{W: 0, Sec: 1.0}) || c0.Points[2] != (TimelinePoint{W: 2, Sec: 3.0}) {
		t.Fatalf("ch0 points wrong: %+v", c0.Points)
	}
	// Chapter 1: only word 3 (chapter-relative 0) at 4s is aligned.
	c1 := p.Timeline[1]
	if len(c1.Points) != 1 || c1.Points[0] != (TimelinePoint{W: 0, Sec: 4.0}) {
		t.Fatalf("ch1 points wrong: %+v", c1.Points)
	}
}

func TestBuildRenderTimeline_ParagraphSparseAndMonotonic(t *testing.T) {
	// Paragraph mode: anchors come from segment start/end. Second segment's
	// start sec is lower than the prior end (out-of-order) → must clamp.
	p := &AnchorAlignmentPayload{
		Unit:          "paragraph",
		EbookChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 100}},
		Segments: []Segment{
			{EbookStart: 0, EbookEnd: 10, Kind: SegAligned, StartSec: 1.0, EndSec: 5.0},
			{EbookStart: 20, EbookEnd: 30, Kind: SegAligned, StartSec: 4.0, EndSec: 9.0}, // 4.0 < 5.0
		},
	}
	buildRenderTimeline(p)

	if len(p.Timeline) != 1 {
		t.Fatalf("want 1 timeline, got %d", len(p.Timeline))
	}
	pts := p.Timeline[0].Points
	// Monotonic non-decreasing in both W and Sec.
	for i := 1; i < len(pts); i++ {
		if pts[i].W < pts[i-1].W {
			t.Fatalf("W not sorted: %+v", pts)
		}
		if pts[i].Sec < pts[i-1].Sec {
			t.Fatalf("Sec not monotonic (clamp failed): %+v", pts)
		}
	}
}

func TestBuildRenderTimeline_SkipsUntimedAndEmpty(t *testing.T) {
	// No baked times anywhere → no timelines (degrade to no-follow).
	p := &AnchorAlignmentPayload{
		Unit:          "word",
		EbookChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 5}},
		Segments: []Segment{
			{EbookStart: 0, EbookEnd: 5, Kind: SegAligned}, // no StartSec/WordSecs
			{EbookStart: 0, EbookEnd: 5, Kind: SegReplace, StartSec: 2.0},
		},
	}
	buildRenderTimeline(p)
	if len(p.Timeline) != 0 {
		t.Fatalf("want no timelines for untimed segments, got %+v", p.Timeline)
	}
}
