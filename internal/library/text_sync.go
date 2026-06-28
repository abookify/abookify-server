package library

import (
	"encoding/json"
	"sort"

	"github.com/pj/abookify/internal/db"
)

// Reader follow-mode rendering support (#210). The reader picks a render mode
// per displayed source from the alignment method+unit: word-by-word karaoke
// for the transcript / word-anchor ebooks (driven by sync_data, unchanged),
// and PARAGRAPH-level follow for ebooks that align to the narration only by
// paragraph/embedding (a different translation — words don't correspond, so a
// coarser paragraph highlight is the appropriate mode). BuildTextSync produces
// the per-paragraph audio time windows that drive that paragraph follow.
//
// This is a read-only consumer of transcription's alignment payload (like
// diff.go) — it maps the baked segment times onto the displayed ebook's
// paragraphs. Basis-robust: segment offsets (chunker/Tokenize basis) and
// paragraph offsets (Fields basis) are each normalized to a 0..1 position
// WITHIN the chapter, then a piecewise-linear frac→time map (anchored on the
// aligned segments) assigns each paragraph a [start,end] in audio seconds.

// TextSyncSpan is one paragraph's audio time window (seconds, on the same
// continuous transcript/audio timeline the reader's karaoke clock uses).
type TextSyncSpan struct {
	ParagraphIdx int     `json:"p"`
	Start        float64 `json:"s"`
	End          float64 `json:"e"`
}

// TextSync is GET /api/works/{id}/text-sync/{bookId}/{chapterIdx}. mode mirrors
// the client resolver (word|paragraph|none); spans are populated only for the
// paragraph mode (word mode is driven by sync_data client-side).
type TextSync struct {
	Mode       string         `json:"mode"`
	Method     string         `json:"method"`
	Unit       string         `json:"unit"`
	Confidence float64        `json:"confidence"`
	Spans      []TextSyncSpan `json:"spans"`
}

type fracAnchor struct {
	frac float64
	sec  float64
}

// BuildTextSync resolves the displayed source's follow mode and, for the
// paragraph case, the per-paragraph time windows for one chapter.
func BuildTextSync(store *db.Store, workID, bookID int64, chapterIdx int) (*TextSync, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return &TextSync{Mode: "none"}, err
	}
	// Transcript displayed → word-by-word (client uses sync_data; no spans).
	transIDs := map[int64]bool{}
	for _, b := range work.TextFiles {
		if b.Origin == "whisper_transcript" || b.Format == "transcript" {
			transIDs[b.ID] = true
		}
	}
	if transIDs[bookID] {
		return &TextSync{Mode: "word", Method: "transcript", Unit: "word", Confidence: 1}, nil
	}

	aligns, err := store.ListAlignmentsForWork(workID)
	if err != nil {
		return &TextSync{Mode: "none"}, err
	}
	// Best row pairing this ebook with any transcript (collapse dual rows).
	var best *db.Alignment
	for i := range aligns {
		a := &aligns[i]
		paired := (a.FromBookID == bookID && transIDs[a.ToBookID]) ||
			(a.ToBookID == bookID && transIDs[a.FromBookID])
		if !paired {
			continue
		}
		if best == nil || a.Confidence > best.Confidence {
			best = a
		}
	}
	if best == nil {
		return &TextSync{Mode: "none"}, nil
	}

	// The reader renders an EBOOK by paragraph-follow regardless of the row's
	// unit: word-by-word karaoke on an ebook needs a composed word map we don't
	// have yet, but a word-ANCHOR alignment still yields fine-grained paragraph
	// times (many small segments → accurate anchors), so same-edition ebooks
	// get a tight follow and cross-translation (embedding) a coarser one. The
	// transcript source is the only word-by-word path (handled above). Method/
	// Unit still report the underlying alignment for transparency.
	out := &TextSync{Mode: "paragraph", Method: best.Method, Unit: best.Unit, Confidence: best.Confidence}

	var p AnchorAlignmentPayload
	if json.Unmarshal([]byte(best.Pairs), &p) != nil {
		return out, nil
	}

	// The ebook is the FROM side for the alignment rows; if this row was stored
	// transcript→ebook, the ebook offsets live on the To side. EbookChapters /
	// segment es/ee always describe the ebook side regardless, so use them.
	var cStart, cLen int
	found := false
	for _, cs := range p.EbookChapters {
		if cs.Index == chapterIdx {
			cStart, cLen = cs.Start, cs.Len
			found = true
			break
		}
	}
	if !found || cLen <= 0 {
		return out, nil
	}
	cEnd := cStart + cLen

	// Frac→time anchors from aligned segments overlapping this chapter.
	var anchors []fracAnchor
	for _, s := range p.Segments {
		if s.Kind != SegAligned || s.EndSec <= 0 {
			continue
		}
		if s.EbookEnd <= cStart || s.EbookStart >= cEnd {
			continue // segment is outside this chapter
		}
		fs := clamp01(float64(s.EbookStart-cStart) / float64(cLen))
		fe := clamp01(float64(s.EbookEnd-cStart) / float64(cLen))
		anchors = append(anchors, fracAnchor{fs, s.StartSec}, fracAnchor{fe, s.EndSec})
	}
	if len(anchors) < 2 {
		return out, nil // not enough timing to follow this chapter
	}
	sort.Slice(anchors, func(i, j int) bool {
		if anchors[i].frac == anchors[j].frac {
			return anchors[i].sec < anchors[j].sec
		}
		return anchors[i].frac < anchors[j].frac
	})

	paras, err := store.ListParagraphs(bookID, chapterIdx)
	if err != nil || len(paras) == 0 {
		return out, nil
	}
	totalWords := 0
	for _, pa := range paras {
		if pa.WordEnd > totalWords {
			totalWords = pa.WordEnd
		}
	}
	if totalWords <= 0 {
		return out, nil
	}

	spans := make([]TextSyncSpan, 0, len(paras))
	var prevEnd float64
	for _, pa := range paras {
		fs := clamp01(float64(pa.WordStart) / float64(totalWords))
		fe := clamp01(float64(pa.WordEnd) / float64(totalWords))
		st := interpFrac(anchors, fs)
		en := interpFrac(anchors, fe)
		// Keep the windows monotonic + non-empty so the client's "paragraph
		// whose window contains t" lookup is stable.
		if st < prevEnd {
			st = prevEnd
		}
		if en < st {
			en = st
		}
		spans = append(spans, TextSyncSpan{ParagraphIdx: pa.ParagraphIdx, Start: st, End: en})
		prevEnd = en
	}
	out.Spans = spans
	return out, nil
}

// SyncWord mirrors a transcript sync_data entry ({w,s,e}) so the reader can
// drive ebook word-by-word karaoke through the SAME path the transcript uses.
type SyncWord struct {
	W string  `json:"w"`
	S float64 `json:"s"`
	E float64 `json:"e"`
}

// BuildEbookWordSync composes a per-word audio map for one chapter of a
// word-anchor-aligned ebook (#210b): each readable ebook word with its audio
// time, in chapter order — the "composed alignment" that lets the EBOOK side
// highlight word-by-word like the transcript, instead of paragraph-follow.
// Returns nil when the source isn't a word-anchor ebook or the chapter has no
// per-word timing.
//
// Readable words come from displayTokenize on the SAME chapter text the
// aligner tokenized (loadContentChapters), so they're index-aligned with the
// payload's word offsets. Aligned segments carry WordSecs (per-ebook-word start
// sec); unaligned (skipped) words forward-fill the previous time so the array
// stays monotonic for the karaoke binary-search.
func BuildEbookWordSync(store *db.Store, workID, bookID int64, chapterIdx int) ([]SyncWord, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return nil, err
	}
	transIDs := map[int64]bool{}
	for _, b := range work.TextFiles {
		if b.Origin == "whisper_transcript" || b.Format == "transcript" {
			transIDs[b.ID] = true
		}
	}
	if transIDs[bookID] {
		return nil, nil // transcript drives its own sync_data
	}
	aligns, err := store.ListAlignmentsForWork(workID)
	if err != nil {
		return nil, err
	}
	var best *db.Alignment
	for i := range aligns {
		a := &aligns[i]
		if a.Unit != "word" {
			continue
		}
		paired := (a.FromBookID == bookID && transIDs[a.ToBookID]) ||
			(a.ToBookID == bookID && transIDs[a.FromBookID])
		if !paired {
			continue
		}
		if best == nil || a.Confidence > best.Confidence {
			best = a
		}
	}
	if best == nil {
		return nil, nil
	}
	var p AnchorAlignmentPayload
	if json.Unmarshal([]byte(best.Pairs), &p) != nil {
		return nil, nil
	}
	var cStart, cLen int
	found := false
	for _, cs := range p.EbookChapters {
		if cs.Index == chapterIdx {
			cStart, cLen = cs.Start, cs.Len
			found = true
			break
		}
	}
	if !found || cLen <= 0 {
		return nil, nil
	}

	// Readable words for this chapter, index-aligned with the aligner's tokens.
	chapters, err := loadContentChapters(store, bookID, true)
	if err != nil {
		return nil, err
	}
	var words []string
	for _, ch := range chapters {
		if ch.Index == chapterIdx {
			words = displayTokenize(ch.Text)
			break
		}
	}
	if len(words) == 0 {
		return nil, nil
	}
	if len(words) > cLen { // tolerate a tiny tokenizer drift; never index OOB
		words = words[:cLen]
	}
	n := len(words)
	cEnd := cStart + cLen

	// Per-word start times from the aligned segments' WordSecs.
	times := make([]float64, n)
	known := make([]bool, n)
	anyKnown := false
	for _, s := range p.Segments {
		if s.Kind != SegAligned || len(s.WordSecs) == 0 {
			continue
		}
		if s.EbookEnd <= cStart || s.EbookStart >= cEnd {
			continue
		}
		for j, sec := range s.WordSecs {
			local := (s.EbookStart + j) - cStart
			if local < 0 || local >= n {
				continue
			}
			times[local] = sec
			known[local] = true
			anyKnown = true
		}
	}
	if !anyKnown {
		return nil, nil
	}
	// Forward-fill skipped (unaligned) words so times are monotonic; back-fill
	// any leading gap with the first known time.
	var firstKnown float64
	for i := 0; i < n; i++ {
		if known[i] {
			firstKnown = times[i]
			break
		}
	}
	last := firstKnown
	for i := 0; i < n; i++ {
		if known[i] {
			last = times[i]
		} else {
			times[i] = last
		}
	}

	out := make([]SyncWord, n)
	for i := 0; i < n; i++ {
		end := times[i] + 0.3
		if i+1 < n && times[i+1] > times[i] {
			end = times[i+1]
		}
		out[i] = SyncWord{W: words[i], S: times[i], E: end}
	}
	return out, nil
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// interpFrac linearly interpolates an audio time for a chapter-fraction from
// the sorted (frac,sec) anchors. Clamps to the first/last anchor outside range.
func interpFrac(anchors []fracAnchor, frac float64) float64 {
	if len(anchors) == 0 {
		return 0
	}
	if frac <= anchors[0].frac {
		return anchors[0].sec
	}
	last := anchors[len(anchors)-1]
	if frac >= last.frac {
		return last.sec
	}
	for i := 1; i < len(anchors); i++ {
		a, b := anchors[i-1], anchors[i]
		if frac <= b.frac {
			span := b.frac - a.frac
			if span <= 0 {
				return a.sec
			}
			t := (frac - a.frac) / span
			return a.sec + t*(b.sec-a.sec)
		}
	}
	return last.sec
}
