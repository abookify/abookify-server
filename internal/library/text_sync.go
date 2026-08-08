package library

import (
	"encoding/json"
	"fmt"
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
		// A transcript is word-timed STT output, so its default mode is word-by-word.
		// But only claim mode=word when this chapter ACTUALLY has retrievable word
		// timing — a chapter with an empty map is a lie the reader silently swallows
		// (text-sync promises karaoke, the map endpoint delivers nothing). Today every
		// live transcript chapter has timing, so this is a guard against a future
		// wordless/imported transcript rather than a live repair — but the endpoint
		// must never promise word level it can't deliver.
		if wm, _ := BuildTranscriptWordSync(store, workID, bookID, chapterIdx); len(wm) > 0 {
			return &TextSync{Mode: "word", Method: "transcript", Unit: "word", Confidence: 1}, nil
		}
		return &TextSync{Mode: "none", Method: "transcript", Unit: "word"}, nil
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
		// No alignment rows at all — the work may still be word-synced BY
		// CONSTRUCTION through its own TTS edition (sync words == this ebook's
		// words; no aligner ever ran because none was needed).
		if wm, err := BuildTTSEditionWordSync(store, workID, bookID, chapterIdx); err == nil && len(wm) > 0 {
			return &TextSync{Mode: "word", Method: "tts", Unit: "word", Confidence: 1}, nil
		}
		return &TextSync{Mode: "none"}, nil
	}

	// Prefer word-by-word karaoke whenever a composed per-word audio map exists for
	// THIS chapter — regardless of which alignment row is highest-confidence. A
	// word-anchor alignment can coexist with a higher-confidence paragraph
	// (embedding) row (e.g. Plato's Republic: a usable word map alongside a
	// near-zero paragraph row); the word map is the finer sync, so it wins.
	// BuildEbookWordSync picks the best WORD alignment itself and returns empty when
	// there's no usable per-word timing (e.g. Meditations, which then correctly stays
	// on its healthy paragraph path). BUG FIXED: gating this on best.Unit=="word"
	// meant a paragraph-best work with a valid word map reported mode=paragraph, then
	// built 0 spans → dead text that never highlighted (Republic, and word chapters
	// of works whose top row is paragraph).
	if wm, err := BuildEbookWordSync(store, workID, bookID, chapterIdx); err == nil && len(wm) > 0 {
		return &TextSync{Mode: "word", Method: best.Method, Unit: "word", Confidence: best.Confidence}, nil
	}

	// No word map for this chapter. Try paragraph-follow; if we can't build any
	// spans (no timing/coverage — front matter, an un-narrated section, or a broken
	// paragraph row), report Mode "none" so the reader shows "no audio sync"
	// HONESTLY, instead of empty paragraph-follow that never highlights and reads as
	// "sync is broken". `none` is returned from every no-spans exit below.
	none := &TextSync{Mode: "none", Method: best.Method, Unit: best.Unit, Confidence: best.Confidence}
	out := &TextSync{Mode: "paragraph", Method: best.Method, Unit: best.Unit, Confidence: best.Confidence}

	var p AnchorAlignmentPayload
	if json.Unmarshal([]byte(best.Pairs), &p) != nil {
		return none, nil
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
		return none, nil // this chapter isn't in the alignment — no sync
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
		return none, nil // not enough timing to follow this chapter — no sync
	}
	sort.Slice(anchors, func(i, j int) bool {
		if anchors[i].frac == anchors[j].frac {
			return anchors[i].sec < anchors[j].sec
		}
		return anchors[i].frac < anchors[j].frac
	})

	paras, err := store.ListParagraphs(bookID, chapterIdx)
	if err != nil || len(paras) == 0 {
		return none, nil
	}
	totalWords := 0
	for _, pa := range paras {
		if pa.WordEnd > totalWords {
			totalWords = pa.WordEnd
		}
	}
	if totalWords <= 0 {
		return none, nil
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
	if len(spans) == 0 {
		return none, nil // built nothing usable — honest "no sync" over empty paragraph-follow
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

	// Plausibility guard. A failed alignment (near-zero confidence) can still
	// produce a NON-EMPTY word map whose per-word times are collapsed into a tiny
	// range — Plato's Republic aligns 20k words into <1s, and every chapter reads
	// ~12–25,000 words/sec. Word-by-word karaoke on those times is worse than none:
	// it "highlights" nonsensically. If the composed map implies an impossible
	// narration rate, reject it (return empty) so BuildTextSync falls back to
	// paragraph, then to an honest mode "none". Real narration is ~2–3 words/sec; a
	// SLOW map (sparse anchors over a long chapter) is fine — only collapsed/too-fast
	// is garbage. (Counterpart to the mode-precedence fix: prefer the word map, but
	// only when it's actually usable.)
	if n >= minWordsForRateCheck {
		span := out[n-1].S - out[0].S
		if span <= 0 || float64(n)/span > maxPlausibleWordsPerSec {
			return nil, nil
		}
	}
	return out, nil
}

const (
	minWordsForRateCheck    = 30 // don't rate-check tiny maps (a heading / one-line page)
	maxPlausibleWordsPerSec = 8  // above this the per-word times are collapsed/garbage (real narration ~2–3)
)

// BuildTranscriptWordSync composes the per-word audio map for one chapter of a
// displayed TRANSCRIPT (the Whisper output itself). The transcript's word timing
// lives in sync_data (keyed by audio book) as one continuous blob on the same
// audio timeline as the transcript chapters' start/end secs; this returns the
// words whose timestamps fall inside the chapter's [start,end) window — exactly
// the slice the reader filters out of the blob per chapter. Empty (nil) when the
// work has no sync_data or this chapter has no words, so callers can tell an
// honest "no word timing here" from a real map. This is the transcript
// counterpart to BuildEbookWordSync so BOTH word-mode sources answer the same
// /word-sync endpoint with a real map (mobile hit an empty map calling that
// endpoint for a transcript, since BuildEbookWordSync deliberately bails).
func BuildTranscriptWordSync(store *db.Store, workID, bookID int64, chapterIdx int) ([]SyncWord, error) {
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return nil, err
	}
	var start, end float64
	found := false
	for _, ch := range chapters {
		if ch.Index == chapterIdx {
			start, end, found = ch.StartSec, ch.EndSec, true
			break
		}
	}
	if !found {
		return nil, nil
	}
	all, err := LoadWorkSyncWords(store, workID)
	if err != nil || len(all) == 0 {
		return nil, nil
	}
	return SliceTranscriptChapter(all, start, end), nil
}

// LoadWorkSyncWords parses every sync_data row for a work into one continuous
// []SyncWord (the transcript's word timeline). Parsing the (potentially large)
// blob is the expensive step, so callers that inspect many chapters of the same
// transcript should load ONCE and reuse it with SliceTranscriptChapter rather
// than calling BuildTranscriptWordSync per chapter (which re-parses each time).
func LoadWorkSyncWords(store *db.Store, workID int64) ([]SyncWord, error) {
	rows, err := store.ListSyncForWork(workID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var all []SyncWord
	for _, r := range rows {
		var ws []SyncWord
		if json.Unmarshal([]byte(r.Timestamps), &ws) != nil {
			continue
		}
		all = append(all, ws...)
	}
	return all, nil
}

// SliceTranscriptChapter returns the words overlapping a chapter's [start,end)
// audio window. A single-chapter transcript carries no per-chapter bounds
// (start==end==0) — then the whole blob IS the chapter. Returns nil when nothing
// overlaps, the honest "no word timing here" signal.
func SliceTranscriptChapter(all []SyncWord, start, end float64) []SyncWord {
	if len(all) == 0 {
		return nil
	}
	if end <= start {
		return all
	}
	out := make([]SyncWord, 0, len(all))
	for _, w := range all {
		if w.E > start && w.S < end {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// BuildDisplayWordSync returns the per-word audio map for ANY displayed word-mode
// source — the transcript (BuildTranscriptWordSync) or a word-anchor ebook
// (BuildEbookWordSync) — so the /word-sync endpoint delivers a real map whichever
// source the reader shows. Empty (nil) when the source has no per-word timing for
// the chapter, which is the honest "fall back to paragraph / none" signal.
func BuildDisplayWordSync(store *db.Store, workID, bookID int64, chapterIdx int) ([]SyncWord, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return nil, err
	}
	for _, b := range work.TextFiles {
		if b.ID == bookID && (b.Origin == "whisper_transcript" || b.Format == "transcript") {
			return BuildTranscriptWordSync(store, workID, bookID, chapterIdx)
		}
	}
	if wm, err := BuildEbookWordSync(store, workID, bookID, chapterIdx); err != nil || len(wm) > 0 {
		return wm, err
	}
	// No anchor-composed map — the TTS-by-construction path (see
	// BuildTTSEditionWordSync).
	return BuildTTSEditionWordSync(store, workID, bookID, chapterIdx)
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

// BuildTTSEditionWordSync composes the per-word audio map for a displayed
// ebook chapter narrated by the work's OWN TTS edition. A TTS edition needs no
// alignment row: its sync words WERE mapped onto this ebook's chapter text at
// generation (word-synced by construction), and its chapter files are named
// chapter-%03d.mp3 by the ebook chapter index they narrate — definitional, not
// heuristic. Without this, a TTS-only work (the clean Carol sample, Alice,
// Jekyll...) showed "no audio sync" in the reader while carrying perfect
// per-word timing, because mode resolution consulted only alignment rows.
//
// Times are shifted to the edition's continuous play-order timeline (the
// player's clock), and the map is served only when the sync row's word count
// EXACTLY matches the chapter's — the construction guarantee, verified rather
// than assumed.
func BuildTTSEditionWordSync(store *db.Store, workID, bookID int64, chapterIdx int) ([]SyncWord, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return nil, err
	}
	for _, b := range work.TextFiles {
		if b.ID == bookID && (b.Origin == "whisper_transcript" || b.Format == "transcript") {
			return nil, nil // transcripts have their own path
		}
	}
	type ttsFile struct {
		book *db.Book
		idx  int // ebook chapter index from the chapter-%03d filename
	}
	var files []ttsFile
	for i := range work.AudioFiles {
		b := &work.AudioFiles[i]
		if b.Origin != "tts_kokoro" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(b.Filename, "chapter-%d.", &n); err != nil {
			continue
		}
		files = append(files, ttsFile{book: b, idx: n})
	}
	if len(files) == 0 {
		return nil, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].idx < files[j].idx })

	var target *ttsFile
	offset := 0.0 // continuous timeline position of the target file (play order)
	for i := range files {
		if files[i].idx == chapterIdx {
			target = &files[i]
			break
		}
		offset += files[i].book.Duration
	}
	if target == nil {
		return nil, nil
	}

	var wc int
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return nil, err
	}
	found := false
	for _, ch := range chapters {
		if ch.Index == chapterIdx {
			wc, found = ch.WordCount, true
			break
		}
	}
	if !found || wc == 0 {
		return nil, nil
	}

	rows, err := store.ListSyncForWork(workID)
	if err != nil {
		return nil, err
	}
	var words []SyncWord
	for _, r := range rows {
		if r.AudioBookID != target.book.ID {
			continue
		}
		var ws []SyncWord
		if json.Unmarshal([]byte(r.Timestamps), &ws) != nil {
			continue
		}
		if len(ws) > len(words) {
			words = ws
		}
	}
	// The construction guarantee, checked: every displayed word has a
	// timestamp because the audio was generated from these exact words. A
	// mismatch means this sync was NOT built from this text — serve nothing
	// rather than a map that drifts.
	if len(words) != wc {
		return nil, nil
	}
	out := make([]SyncWord, len(words))
	for i, w := range words {
		out[i] = SyncWord{W: w.W, S: w.S + offset, E: w.E + offset}
	}
	return out, nil
}
