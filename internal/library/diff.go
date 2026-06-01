package library

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// DiffSource identifies one side of a source comparison.
type DiffSource struct {
	BookID int64  `json:"book_id"`
	Origin string `json:"origin"`
	Label  string `json:"label"`
}

// DiffSpan is one run in reading order. kind ∈ aligned|ebook-only|trans-only|
// replace. For aligned runs a_text/b_text are empty (the run is summarized by
// a_words/b_words) to bound the payload; divergent runs carry the actual text
// (capped). a_pct/b_pct locate the run as 0–1 position-through-source.
type DiffSpan struct {
	Kind   string  `json:"kind"`
	AText  string  `json:"a_text"`
	BText  string  `json:"b_text"`
	AWords int     `json:"a_words,omitempty"`
	BWords int     `json:"b_words,omitempty"`
	APct   float64 `json:"a_pct"`
	BPct   float64 `json:"b_pct"`
}

// WorkDiff is the GET /api/works/{id}/diff payload (contract in
// SESSION_HANDOFF.md — mobile's MeldScreen consumes this shape).
type WorkDiff struct {
	SourceA  DiffSource `json:"source_a"`
	SourceB  DiffSource `json:"source_b"`
	Coverage float64    `json:"coverage"`
	Method   string     `json:"method"`
	Spans    []DiffSpan `json:"spans"`
}

// maxSpanWords caps each side of a divergent span so a single large skip /
// ad-lib can't blow up the response. Truncated text gets an ellipsis marker.
const maxSpanWords = 600

// displayNonWord mirrors anchor.go's nonWord but ALSO keeps A-Z. Tokenize
// lowercases before applying nonWord, and lowercasing is a 1:1 positional
// char map (letters→letters, both kept), so applying this case-preserving
// variant to the original text yields the SAME token boundaries/count as
// Tokenize — letting us recover readable, original-case span text from the
// alignment's word offsets.
var displayNonWord = regexp.MustCompile(`[^A-Za-z0-9' ]+`)

func displayTokenize(s string) []string {
	s = strings.ReplaceAll(s, "’", "'")
	s = displayNonWord.ReplaceAllString(s, " ")
	return strings.Fields(s)
}

// assembleDisplay concatenates case-preserving tokens across chapters, index-
// aligned with AssembleStream's Tokenize stream (same per-chapter counts).
func assembleDisplay(chapters []ChapterText) []string {
	var toks []string
	for _, ch := range chapters {
		toks = append(toks, displayTokenize(ch.Text)...)
	}
	return toks
}

// joinCapped joins toks[start:end] (clamped) into display text, capping at
// maxSpanWords with a trailing marker so a huge run stays bounded.
func joinCapped(toks []string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(toks) {
		end = len(toks)
	}
	if start >= end {
		return ""
	}
	n := end - start
	if n > maxSpanWords {
		shown := strings.Join(toks[start:start+maxSpanWords], " ")
		return shown + " … [" + itoa(n-maxSpanWords) + " more words]"
	}
	return strings.Join(toks[start:end], " ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func bookLabel(b *db.Book) string {
	if b == nil {
		return ""
	}
	if b.Edition != "" && b.Title != "" {
		return b.Title + " — " + b.Edition
	}
	if b.Title != "" {
		return b.Title
	}
	return b.Filename
}

// BuildDiff assembles the render-ready source comparison for a work from its
// best word-level alignment. found=false (→ 404) when the work has no
// word-unit cross-source alignment with segments. Re-derives the exact token
// streams the offsets index into, so the span text is faithful.
func BuildDiff(store *db.Store, workID int64) (*WorkDiff, bool, error) {
	aligns, err := store.ListAlignmentsForWork(workID)
	if err != nil {
		return nil, false, err
	}
	// Pick the highest-coverage word-unit alignment that carries segments.
	var best *db.Alignment
	var bestPayload AnchorAlignmentPayload
	for i := range aligns {
		a := &aligns[i]
		if a.Unit != "word" {
			continue // embedding/paragraph offsets aren't word-token indices
		}
		var p AnchorAlignmentPayload
		if json.Unmarshal([]byte(a.Pairs), &p) != nil || len(p.Segments) == 0 {
			continue
		}
		if best == nil || a.Confidence > best.Confidence {
			best = a
			bestPayload = p
		}
	}
	if best == nil {
		return nil, false, nil
	}

	ebook, _ := store.GetBook(best.FromBookID)
	trans, _ := store.GetBook(best.ToBookID)

	ebookChapters, err := loadContentChapters(store, best.FromBookID, true)
	if err != nil {
		return nil, false, err
	}
	transChapters, err := loadContentChapters(store, best.ToBookID, false)
	if err != nil {
		return nil, false, err
	}
	ebookToks := assembleDisplay(ebookChapters)
	transToks := assembleDisplay(transChapters)

	ebookWords := bestPayload.EbookWords
	if ebookWords == 0 {
		ebookWords = len(ebookToks)
	}
	transWords := bestPayload.TransWords
	if transWords == 0 {
		transWords = len(transToks)
	}

	pct := func(pos, total int) float64 {
		if total <= 0 {
			return 0
		}
		return float64(pos) / float64(total)
	}

	spans := make([]DiffSpan, 0, len(bestPayload.Segments))
	for _, s := range bestPayload.Segments {
		span := DiffSpan{
			Kind:   string(s.Kind),
			AWords: s.EbookEnd - s.EbookStart,
			BWords: s.TransEnd - s.TransStart,
			APct:   pct(s.EbookStart, ebookWords),
			BPct:   pct(s.TransStart, transWords),
		}
		// Aligned runs are summarized by counts; text omitted to bound payload.
		if s.Kind != SegAligned {
			span.AText = joinCapped(ebookToks, s.EbookStart, s.EbookEnd)
			span.BText = joinCapped(transToks, s.TransStart, s.TransEnd)
		}
		spans = append(spans, span)
	}

	return &WorkDiff{
		SourceA:  DiffSource{BookID: best.FromBookID, Origin: originOf(ebook), Label: bookLabel(ebook)},
		SourceB:  DiffSource{BookID: best.ToBookID, Origin: originOf(trans), Label: bookLabel(trans)},
		Coverage: best.Confidence,
		Method:   best.Method,
		Spans:    spans,
	}, true, nil
}

func originOf(b *db.Book) string {
	if b == nil {
		return ""
	}
	return b.Origin
}
