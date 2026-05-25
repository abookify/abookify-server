// Anchor-based alignment between an ebook and a transcript word stream.
//
// Motivation (see docs/epub-informed-transcription.md): chapter-level
// forced alignment fails whenever the two sides don't share chapter
// structure — the common case (memoirs with named sections, multi-volume
// works whose chapter numbers reset, ebooks padded with publisher/Gutenberg
// front-matter). Empirically the *content* still matches near word-for-word
// (~99% by word count, ~80% of 4-grams are unique 1:1 anchors), so we can
// align the full word streams directly and let structure fall out of the
// alignment instead of being a precondition.
//
// Algorithm:
//  1. Tokenize + normalize both sides.
//  2. Find anchors: n-grams that occur exactly once in the ebook ("hapax")
//     and at least once in the transcript. Each transcript occurrence is a
//     candidate (ebookPos, transPos) pair.
//  3. Keep the longest monotonically-increasing chain of anchors (LIS on
//     transcript position, in ebook order). This enforces ordering and
//     discards false-positive matches from repeated phrases.
//  4. Classify the gaps between consecutive anchors: aligned (both sides
//     advance together), ebook-only (text the audio skipped), trans-only
//     (narrator ad-lib / intro / outro). Divergence detection falls out
//     of the gap classification for free.
//
// All functions here are pure (operate on token slices) so they can be
// unit-tested against synthetic sequences with known-correct alignments.
package library

import (
	"regexp"
	"sort"
	"strings"
)

// Anchor ties an ebook word position to a transcript word position. Both
// are indices into the respective normalized token slices (the start of a
// matched n-gram).
type Anchor struct {
	EbookPos int
	TransPos int
}

// SegmentKind classifies a span between (or around) anchors.
type SegmentKind string

const (
	SegAligned   SegmentKind = "aligned"    // both sides advance together
	SegEbookOnly SegmentKind = "ebook-only" // ebook text with no transcript counterpart (audio skipped it)
	SegTransOnly SegmentKind = "trans-only" // transcript text with no ebook counterpart (narrator ad-lib/intro/outro)
	SegReplace   SegmentKind = "replace"    // both sides have content but it differs (STT noise / edition divergence)
)

// Segment is a half-open span [Start,End) on each side, in global token
// offsets over the concatenated content streams Align ran on. StartSec/
// EndSec/WordSecs are the baked audio timeline (resolved at write from
// sync_data); they're set only on aligned segments — see bakeSegmentTimes.
type Segment struct {
	EbookStart int         `json:"es"`
	EbookEnd   int         `json:"ee"`
	TransStart int         `json:"ts"`
	TransEnd   int         `json:"te"`
	Kind       SegmentKind `json:"k"`
	StartSec   float64     `json:"ss,omitempty"` // audio start sec of this segment (aligned only)
	EndSec     float64     `json:"se,omitempty"` // audio end sec (aligned only)
	WordSecs   []float64   `json:"ws,omitempty"` // per-ebook-word start sec; len==ee-es; word path only
}

// Alignment is the result: the monotonic anchor chain plus the classified
// gaps that cover everything between and around the anchors.
type Alignment struct {
	Anchors  []Anchor
	Segments []Segment
}

// ChapterSpan records where one source chapter lands in the concatenated
// token stream that Align operates on. Lets a consumer map a global word
// offset back to (chapter index, word-within-chapter) — and from there, via
// the transcript's per-chapter sync_data, to an audio timestamp.
type ChapterSpan struct {
	Index int `json:"idx"`   // chapter index in the source book
	Start int `json:"start"` // global token offset where this chapter begins
	Len   int `json:"len"`   // token count in this chapter
}

// boilerplateExactTitles are chapter titles that, matched whole (case-
// insensitive, trimmed), are publisher/archive front- and back-matter rather
// than narrated content. Excluded from alignment so they don't drift cross-
// source word offsets or surface as spurious "ebook-only" divergences.
var boilerplateExactTitles = map[string]bool{
	"contents": true, "table of contents": true,
	"index": true, "notes": true, "end notes": true, "endnotes": true,
	"footnotes": true, "references": true, "bibliography": true,
	"acknowledgments": true, "acknowledgements": true, "about the author": true,
	"copyright": true, "colophon": true, "a note on the type": true,
	"illustration permissions": true, "permissions": true, "title page": true,
	"cover": true, "dedication": true, "epigraph": true, "imprint": true,
	"frontispiece": true,
}

// IsBoilerplateChapterTitle reports whether a chapter title looks like
// non-narrative front/back-matter that should be excluded from alignment.
// "Project Gutenberg" headers/licenses carry a long trailing title, so they
// match as a substring; the rest match the whole (trimmed) title so a real
// chapter like "Notes from Underground" isn't mistaken for an endnotes
// section.
func IsBoilerplateChapterTitle(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return false
	}
	if strings.Contains(t, "project gutenberg") {
		return true
	}
	return boilerplateExactTitles[t]
}

// ChapterText pairs a source chapter index with its plain text. Build these
// from the book's content chapters (boilerplate already filtered out).
type ChapterText struct {
	Index int
	Text  string
}

// AssembleStream tokenizes each chapter and concatenates into one stream,
// returning the tokens plus the ChapterSpan table describing where each
// chapter sits in the stream. The spans are what let global alignment
// offsets be mapped back to (chapter, local-word) on either side.
func AssembleStream(chapters []ChapterText) ([]string, []ChapterSpan) {
	var toks []string
	var spans []ChapterSpan
	for _, ch := range chapters {
		ct := Tokenize(ch.Text)
		if len(ct) == 0 {
			continue
		}
		spans = append(spans, ChapterSpan{Index: ch.Index, Start: len(toks), Len: len(ct)})
		toks = append(toks, ct...)
	}
	return toks, spans
}

// LocateGlobal maps a global token offset to (chapter index, word-within-
// chapter). Returns ok=false if the offset falls outside every span (e.g.,
// past the end). Spans must be sorted by Start (AssembleStream guarantees it).
func LocateGlobal(spans []ChapterSpan, globalPos int) (chapterIdx, localPos int, ok bool) {
	for _, s := range spans {
		if globalPos >= s.Start && globalPos < s.Start+s.Len {
			return s.Index, globalPos - s.Start, true
		}
	}
	return 0, 0, false
}

var nonWord = regexp.MustCompile(`[^a-z0-9' ]+`)

// Tokenize normalizes text to lowercase alphanumeric words. Punctuation is
// dropped; apostrophes are kept so contractions stay one token. This is the
// same normalization used to compare ebook and transcript words.
func Tokenize(s string) []string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "’", "'") // curly apostrophe → straight
	s = nonWord.ReplaceAllString(s, " ")
	return strings.Fields(s)
}

// ngramIndex maps each n-gram to the list of start positions where it occurs.
func ngramIndex(toks []string, n int) map[string][]int {
	idx := make(map[string][]int)
	if n <= 0 || len(toks) < n {
		return idx
	}
	for i := 0; i+n <= len(toks); i++ {
		g := strings.Join(toks[i:i+n], " ")
		idx[g] = append(idx[g], i)
	}
	return idx
}

// FindAnchors returns candidate anchors: for every n-gram that occurs
// exactly once in the ebook and at least once in the transcript, one
// candidate per transcript occurrence. A "clean" anchor (the common case)
// has a single transcript occurrence; repeated phrases yield several
// candidates that the monotonic chain step resolves.
func FindAnchors(ebook, trans []string, n int) []Anchor {
	eIdx := ngramIndex(ebook, n)
	tIdx := ngramIndex(trans, n)
	var out []Anchor
	for g, ePos := range eIdx {
		if len(ePos) != 1 {
			continue // not hapax in ebook — ambiguous on the ebook side, skip
		}
		tPos := tIdx[g]
		if len(tPos) == 0 {
			continue // not in transcript
		}
		for _, tp := range tPos {
			out = append(out, Anchor{EbookPos: ePos[0], TransPos: tp})
		}
	}
	return out
}

// MonotonicChain keeps the largest set of anchors whose transcript positions
// strictly increase in ebook order — a longest-increasing-subsequence over
// TransPos after sorting by EbookPos. Sorting ties (same EbookPos, from a
// phrase repeated in the transcript) by TransPos descending guarantees at
// most one anchor per ebook position survives. O(k log k).
func MonotonicChain(cands []Anchor) []Anchor {
	if len(cands) == 0 {
		return nil
	}
	a := make([]Anchor, len(cands))
	copy(a, cands)
	sort.Slice(a, func(i, j int) bool {
		if a[i].EbookPos != a[j].EbookPos {
			return a[i].EbookPos < a[j].EbookPos
		}
		return a[i].TransPos > a[j].TransPos // descending so ties can't both be chosen
	})

	// Patience LIS on TransPos (strictly increasing), with parent pointers.
	tails := []int{}      // tails[k] = index into a of the smallest tail of an increasing seq of length k+1
	parent := make([]int, len(a))
	for i := range parent {
		parent[i] = -1
	}
	for i, an := range a {
		// strictly-increasing: find first tail whose TransPos >= current
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if a[tails[mid]].TransPos < an.TransPos {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo > 0 {
			parent[i] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, i)
		} else {
			tails[lo] = i
		}
	}
	// Reconstruct.
	out := make([]Anchor, len(tails))
	k := tails[len(tails)-1]
	for j := len(tails) - 1; j >= 0; j-- {
		out[j] = a[k]
		k = parent[k]
	}
	return out
}

// classifyGap labels the span between two matched points based on whether
// each side has unmatched tokens in it.
func classifyGap(eLen, tLen int) (SegmentKind, bool) {
	switch {
	case eLen == 0 && tLen == 0:
		return "", false // nothing between — adjacent anchors
	case eLen > 0 && tLen == 0:
		return SegEbookOnly, true
	case eLen == 0 && tLen > 0:
		return SegTransOnly, true
	default:
		return SegReplace, true
	}
}

// Align produces the full alignment between ebook and transcript token
// slices using n-gram anchors. n=4 is the empirical sweet spot. Segments
// cover both sequences start-to-end: matched runs (SegAligned) interleaved
// with divergence gaps.
func Align(ebook, trans []string, n int) Alignment {
	chain := MonotonicChain(FindAnchors(ebook, trans, n))

	var segs []Segment
	ePrev, tPrev := 0, 0
	flushGap := func(eAt, tAt int) {
		if kind, ok := classifyGap(eAt-ePrev, tAt-tPrev); ok {
			segs = append(segs, Segment{EbookStart: ePrev, EbookEnd: eAt, TransStart: tPrev, TransEnd: tAt, Kind: kind})
		}
	}

	i := 0
	for i < len(chain) {
		a := chain[i]
		// Merge this anchor and any directly-consecutive ones (both sides
		// advancing in lockstep, gap 0/0) into one aligned run.
		eCur, tCur := a.EbookPos+1, a.TransPos+1
		j := i + 1
		for j < len(chain) && chain[j].EbookPos == eCur && chain[j].TransPos == tCur {
			eCur++
			tCur++
			j++
		}
		// The matched run spans the merged anchor positions plus the trailing
		// (n-1) tokens of the last n-gram.
		eRunStart, tRunStart := a.EbookPos, a.TransPos
		eEnd := min(eCur-1+n, len(ebook))
		tEnd := min(tCur-1+n, len(trans))
		// Consecutive anchors' n-grams overlap (anchor at pos p covers p..p+n),
		// so a run can start inside the previous run's tail. Clamp the start to
		// the previous end so runs never overlap and gaps stay non-negative.
		if eRunStart < ePrev {
			eRunStart = ePrev
		}
		if tRunStart < tPrev {
			tRunStart = tPrev
		}
		if eEnd <= eRunStart && tEnd <= tRunStart {
			i = j // fully covered by the previous run
			continue
		}
		flushGap(eRunStart, tRunStart)
		segs = append(segs, Segment{EbookStart: eRunStart, EbookEnd: eEnd, TransStart: tRunStart, TransEnd: tEnd, Kind: SegAligned})
		if eEnd > ePrev {
			ePrev = eEnd
		}
		if tEnd > tPrev {
			tPrev = tEnd
		}
		i = j
	}
	// Trailing gap to the end of both sides.
	flushGap(len(ebook), len(trans))

	return Alignment{Anchors: chain, Segments: segs}
}

// Coverage reports the fraction of ebook tokens that land inside an aligned
// segment — a quick health number for an alignment (1.0 = every ebook word
// has a transcript counterpart).
func (a Alignment) Coverage(ebookLen int) float64 {
	if ebookLen == 0 {
		return 0
	}
	covered := 0
	for _, s := range a.Segments {
		if s.Kind == SegAligned {
			covered += s.EbookEnd - s.EbookStart
		}
	}
	if covered > ebookLen {
		covered = ebookLen
	}
	return float64(covered) / float64(ebookLen)
}
