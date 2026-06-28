package library

import "sort"

// Render-ready timeline (#209). The aligned Segments already carry baked audio
// times (StartSec/EndSec/WordSecs), but they're global, sparse, and gap-broken —
// so every consumer (server-web's reader render-mode #210: word karaoke +
// paragraph-follow) had to re-slice them per chapter and rebuild a monotonic
// position→time map. We bake that map ONCE here, per ebook chapter, as a list of
// monotonic (word-offset → second) control points the reader looks up directly.
//
// Coordinated shape (see handoff/transcription.md for server-web):
//   - Points.W   = ebook word offset WITHIN the chapter, in the SAME basis as the
//                  payload's word offsets (Tokenize for anchor, Fields for the
//                  ebook side — index-aligned with displayTokenize on the client).
//   - Points.Sec = book-continuous audio seconds (player currentTime + fileOffset).
//   - Points are sorted by W and monotonic non-decreasing in Sec (clamped).
//   - WORD unit (anchor): dense — a point per aligned ebook word. The client runs
//     word-by-word karaoke straight off it.
//   - PARAGRAPH unit (embedding): sparse — start/end anchors per aligned segment.
//     The client interpolates a paragraph's start word to a second.
//   Empty Points (sparse / unaligned / front-matter chapter) ⇒ omit the chapter
//   (degrade to no-follow), matching #210's "empty spans" behaviour.

// ChapterTimeline is the per-ebook-chapter render-ready position→time map.
type ChapterTimeline struct {
	EbookChapterIdx int             `json:"ci"`
	Unit            string          `json:"unit"` // "word" | "paragraph" (mirrors payload.Unit)
	Points          []TimelinePoint `json:"points"`
}

// TimelinePoint is one monotonic control point: chapter-relative ebook word
// offset → book-continuous audio second.
type TimelinePoint struct {
	W   int     `json:"w"`
	Sec float64 `json:"s"`
}

// buildRenderTimeline derives the per-chapter timeline from the (already
// time-baked) aligned segments. Call AFTER bakeSegmentTimes. No-op when no
// segment carries times (no transcript timeline available).
func buildRenderTimeline(p *AnchorAlignmentPayload) {
	wordMode := p.Unit == "word"
	var timelines []ChapterTimeline

	for _, ch := range p.EbookChapters {
		lo, hi := ch.Start, ch.Start+ch.Len
		if hi <= lo {
			continue
		}
		var pts []TimelinePoint

		for _, s := range p.Segments {
			if s.Kind != SegAligned {
				continue
			}
			if s.EbookEnd <= lo || s.EbookStart >= hi { // no overlap with this chapter
				continue
			}
			if wordMode && len(s.WordSecs) == s.EbookEnd-s.EbookStart {
				// Dense: one point per aligned ebook word in this chapter.
				for i, sec := range s.WordSecs {
					w := s.EbookStart + i
					if w < lo || w >= hi || sec <= 0 {
						continue
					}
					pts = append(pts, TimelinePoint{W: w - lo, Sec: sec})
				}
			} else {
				// Sparse: anchor the segment's start (and end) word to its times.
				if s.StartSec > 0 {
					w := s.EbookStart
					if w < lo {
						w = lo
					}
					pts = append(pts, TimelinePoint{W: w - lo, Sec: s.StartSec})
				}
				if s.EndSec > 0 {
					w := s.EbookEnd - 1
					if w >= hi {
						w = hi - 1
					}
					pts = append(pts, TimelinePoint{W: w - lo, Sec: s.EndSec})
				}
			}
		}

		pts = monotonic(pts)
		if len(pts) == 0 {
			continue
		}
		timelines = append(timelines, ChapterTimeline{
			EbookChapterIdx: ch.Index,
			Unit:            p.Unit,
			Points:          pts,
		})
	}

	p.Timeline = timelines
}

// monotonic sorts by W, keeps one point per W (the earliest second), and clamps
// Sec to be non-decreasing so the client never has to second-guess ordering.
func monotonic(pts []TimelinePoint) []TimelinePoint {
	if len(pts) == 0 {
		return nil
	}
	sort.Slice(pts, func(i, j int) bool {
		if pts[i].W != pts[j].W {
			return pts[i].W < pts[j].W
		}
		return pts[i].Sec < pts[j].Sec
	})
	out := pts[:0]
	lastW, lastSec := -1, 0.0
	for _, p := range pts {
		if p.W == lastW { // dedup: keep the first (smallest) second for a word
			continue
		}
		if p.Sec < lastSec { // clamp non-decreasing
			p.Sec = lastSec
		}
		out = append(out, p)
		lastW, lastSec = p.W, p.Sec
	}
	return out
}
