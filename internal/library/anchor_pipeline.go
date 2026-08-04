// Pipeline glue: run the anchor aligner against a work's ebook + transcript
// books and persist the result into the `alignments` table.
//
// This is the production path that replaces chapter-link forced alignment
// (forced_align.go) for works where the two sides don't share chapter
// structure — i.e. most of them. It aligns the whole word streams and
// records a self-contained payload the reader/diff-UX consumes.
//
// CONTRACT (read by server+web's alignment reader / diff visualization):
// an `alignments` row with Method="anchor", Unit="word", FromBookID=ebook,
// ToBookID=transcript, Confidence=coverage. Its Pairs column is JSON of
// AnchorAlignmentPayload (NOT []db.AlignmentPair — the "anchor" method uses
// this richer shape). Everything needed to render the diff view and to
// project ebook structure onto audio time is in the payload:
//   - EbookChapters / TransChapters: ChapterSpans mapping global word
//     offsets back to (chapter, word-within-chapter) on each side.
//   - Segments: aligned / ebook-only / trans-only / replace spans (global
//     offsets). Divergences are the non-aligned segments.
//   - Coverage + Divergence: summary numbers for the per-work indicator.
//
// To get an audio timestamp for an ebook range: map it through the aligned
// segments to a transcript global offset, use TransChapters to get
// (transcript chapter, local word), then the existing sync_data path
// (GetSyncData / qa.go loadSync) for the time. MapEbookToTrans does the
// first half.
package library

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// DivergenceSummary is the per-work reporting the UI surfaces: how much of
// the ebook the audio covers, and the largest mismatches.
type DivergenceSummary struct {
	AlignedSegs    int `json:"aligned_segs"`
	EbookOnlySegs  int `json:"ebook_only_segs"`
	TransOnlySegs  int `json:"trans_only_segs"`
	ReplaceSegs    int `json:"replace_segs"`
	EbookOnlyWords int `json:"ebook_only_words"` // ebook words with no audio (skipped/boilerplate)
	TransOnlyWords int `json:"trans_only_words"` // transcript words with no ebook (intros/ad-libs)
	// Biggest divergent segments (by combined word span), for the UI to list.
	Top []Segment `json:"top,omitempty"`
}

// AnchorAlignmentPayload is the JSON stored in alignments.pairs for
// Method="anchor" and Method="embedding". Self-contained + render-ready:
// audio times are baked onto aligned segments, so the reader needs only
// this row (no sync_data, no recompute). See SESSION_HANDOFF.md for the
// contract the diff-view/reader build against.
type AnchorAlignmentPayload struct {
	Method        string        `json:"method"` // mirrors the row: "anchor" | "embedding"
	Unit          string        `json:"unit"`   // "word" | "paragraph" → reader render mode
	EbookChapters []ChapterSpan `json:"ebook_chapters"`
	TransChapters []ChapterSpan `json:"trans_chapters"`
	Segments      []Segment     `json:"segments"`
	EbookWords    int           `json:"ebook_words"`
	TransWords    int           `json:"trans_words"`
	Coverage      float64       `json:"coverage"`
	// MatchQuality is the mean cosine similarity of the matched chain, set
	// only by the embedding path (Method="embedding"). High ⇒ same work in a
	// different translation; low everywhere ⇒ genuinely different texts. 0 for
	// the lexical anchor path.
	MatchQuality float64           `json:"match_quality,omitempty"`
	Divergence   DivergenceSummary `json:"divergence"`
	// Timeline is the render-ready, per-ebook-chapter position→time map (#209):
	// monotonic (word-offset → book-continuous audio second) control points the
	// reader (#210) looks up directly instead of recomputing from Segments. Dense
	// for word-anchor, sparse anchors for embedding/paragraph. See render_timeline.go.
	Timeline []ChapterTimeline `json:"timeline,omitempty"`
}

// anchorNGram is the n-gram length used for anchoring. 4 is the empirical
// sweet spot (see docs/epub-informed-transcription.md).
const anchorNGram = 4

// ComputeAnchorAlignment aligns a work's ebook against its transcript with
// the anchor aligner and upserts the result into the alignments table.
// Returns the coverage ratio of the authority pair. No-op (coverage 0, nil)
// if the work lacks either an ebook or a transcript peer.
//
// Beyond the authority pair, every EXISTING anchor row between a publisher
// ebook and a transcript of the work is recomputed too. Re-importing a
// transcript invalidates every alignment row that points at it, not just the
// authority pair's — a work can carry a second epub edition (All Quiet held
// two) or a second transcript (Call of the Wild) — and a row left behind
// describes text that no longer exists, which surfaces as karaoke silently
// broken for whoever displays that pairing. Only rows already present are
// refreshed; no new cross-edition pairs are invented.
func ComputeAnchorAlignment(store *db.Store, workID int64) (float64, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return 0, err
	}

	publishers := map[int64]*db.Book{}
	transcripts := map[int64]*db.Book{}
	var authority, firstTranscript *db.Book
	for i := range work.TextFiles {
		b := &work.TextFiles[i]
		switch b.Origin {
		case "whisper_transcript":
			transcripts[b.ID] = b
			if firstTranscript == nil {
				firstTranscript = b
			}
		case "publisher_epub", "publisher_mobi", "publisher_pdf":
			publishers[b.ID] = b
			if authority == nil || db.OriginAuthority(b.Origin) > db.OriginAuthority(authority.Origin) {
				authority = b
			}
		}
	}
	if authority == nil || firstTranscript == nil {
		return 0, nil // nothing to align
	}

	type bookPair struct{ ebook, trans *db.Book }
	pairs := []bookPair{{authority, firstTranscript}}
	seen := map[[2]int64]bool{{authority.ID, firstTranscript.ID}: true}
	if rows, err := store.ListAlignmentsForWork(workID); err == nil {
		for _, a := range rows {
			e, t := publishers[a.FromBookID], transcripts[a.ToBookID]
			key := [2]int64{a.FromBookID, a.ToBookID}
			if a.Method != "anchor" || e == nil || t == nil || seen[key] {
				continue
			}
			seen[key] = true
			pairs = append(pairs, bookPair{e, t})
		}
	}

	timelines := loadTranscriptTimelines(store, work)
	var primary float64
	for i, p := range pairs {
		cov, err := anchorAlignPair(store, workID, p.ebook, p.trans, timelines)
		if err != nil {
			return primary, err
		}
		if i == 0 {
			primary = cov
		}
	}
	return primary, nil
}

// anchorAlignPair aligns one (publisher ebook, transcript) pair and upserts
// its alignments row. timelines holds every sync blob of the work; the one
// matching this transcript is picked by word count.
func anchorAlignPair(store *db.Store, workID int64, ebook, transcript *db.Book, timelines [][]db.SyncTimestamp) (float64, error) {
	ebookChapters, err := loadContentChapters(store, ebook.ID, true)
	if err != nil {
		return 0, fmt.Errorf("load ebook chapters: %w", err)
	}
	transChapters, err := loadContentChapters(store, transcript.ID, false)
	if err != nil {
		return 0, fmt.Errorf("load transcript chapters: %w", err)
	}

	ebookToks, ebookSpans := AssembleStream(ebookChapters)
	transToks, transSpans := AssembleStream(transChapters)
	if len(ebookToks) == 0 || len(transToks) == 0 {
		return 0, nil
	}

	aln := Align(ebookToks, transToks, anchorNGram)
	coverage := aln.Coverage(len(ebookToks))

	// Bake the audio timeline. The anchor transcript stream is in Tokenize
	// basis; sync_data is in the transcript's whitespace-word (Fields) basis,
	// so map Tokenize offsets → Fields offsets before the timeline lookup.
	tokToFields := buildTokToFields(transChapters)
	fieldCount := 0
	if n := len(tokToFields); n > 0 {
		fieldCount = tokToFields[n-1] + 1
	}
	if timeline := pickTimeline(timelines, fieldCount); len(timeline) > 0 {
		bakeSegmentTimes(aln.Segments, timeline, tokToFields, true)
	}

	payload := AnchorAlignmentPayload{
		Method:        "anchor",
		Unit:          "word",
		EbookChapters: ebookSpans,
		TransChapters: transSpans,
		Segments:      aln.Segments,
		EbookWords:    len(ebookToks),
		TransWords:    len(transToks),
		Coverage:      coverage,
		Divergence:    summarizeAnchorDivergence(aln.Segments),
	}
	buildRenderTimeline(&payload) // #209: bake the per-chapter position→time map
	pairsJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}

	if err := store.SaveAlignment(db.Alignment{
		WorkID:     workID,
		FromBookID: ebook.ID,
		ToBookID:   transcript.ID,
		Unit:       "word",
		Confidence: coverage,
		Method:     "anchor",
		Pairs:      string(pairsJSON),
	}); err != nil {
		return coverage, fmt.Errorf("save alignment: %w", err)
	}
	return coverage, nil
}

// loadContentChapters returns a book's chapters as ChapterText, in index
// order. When dropBoilerplate is set (ebooks), chapters whose title looks
// like publisher/archive front/back-matter are skipped so they don't drift
// the alignment or surface as false divergences.
func loadContentChapters(store *db.Store, bookID int64, dropBoilerplate bool) ([]ChapterText, error) {
	chs, err := store.ListChapters(bookID)
	if err != nil {
		return nil, err
	}
	sort.Slice(chs, func(i, j int) bool { return chs[i].Index < chs[j].Index })
	var out []ChapterText
	for _, ch := range chs {
		if dropBoilerplate && IsBoilerplateChapterTitle(ch.Title) {
			continue
		}
		full, err := store.GetChapterContent(bookID, ch.Index)
		if err != nil || full == nil || full.Content == "" {
			continue
		}
		out = append(out, ChapterText{Index: ch.Index, Text: full.Content})
	}
	return out, nil
}

// summarizeDivergence tallies segment kinds and picks the biggest divergent
// spans for the per-work coverage/divergence report.
func summarizeAnchorDivergence(segs []Segment) DivergenceSummary {
	var d DivergenceSummary
	var diverging []Segment
	for _, s := range segs {
		switch s.Kind {
		case SegAligned:
			d.AlignedSegs++
		case SegEbookOnly:
			d.EbookOnlySegs++
			d.EbookOnlyWords += s.EbookEnd - s.EbookStart
			diverging = append(diverging, s)
		case SegTransOnly:
			d.TransOnlySegs++
			d.TransOnlyWords += s.TransEnd - s.TransStart
			diverging = append(diverging, s)
		case SegReplace:
			d.ReplaceSegs++
			d.EbookOnlyWords += s.EbookEnd - s.EbookStart
			d.TransOnlyWords += s.TransEnd - s.TransStart
			diverging = append(diverging, s)
		}
	}
	sort.Slice(diverging, func(i, j int) bool {
		gi := (diverging[i].EbookEnd - diverging[i].EbookStart) + (diverging[i].TransEnd - diverging[i].TransStart)
		gj := (diverging[j].EbookEnd - diverging[j].EbookStart) + (diverging[j].TransEnd - diverging[j].TransStart)
		return gi > gj
	})
	if len(diverging) > 10 {
		diverging = diverging[:10]
	}
	d.Top = diverging
	return d
}

// loadTranscriptTimelines returns every word-timestamp timeline of the work,
// in reading order (whitespace-word / "Fields" basis — the order sync_data
// stores). The sidecar writes each edition's whole transcript as one
// continuous sync_data blob keyed to that edition's first audio book at
// chapter 0, so a single-edition work yields one timeline and a multi-edition
// work one per edition.
func loadTranscriptTimelines(store *db.Store, work *db.Work) [][]db.SyncTimestamp {
	var out [][]db.SyncTimestamp
	for _, ab := range work.AudioFiles {
		raw, _ := store.GetSyncData(work.ID, ab.ID, 0)
		if raw == "" {
			continue
		}
		var ts []db.SyncTimestamp
		if json.Unmarshal([]byte(raw), &ts) == nil && len(ts) > 0 {
			out = append(out, ts)
		}
	}
	return out
}

// pickTimeline chooses the timeline belonging to a transcript of the given
// whitespace-word count. Nothing ties a sync blob to its transcript book
// directly, but each blob has exactly one timestamp per transcript word, so
// the closest word count is the matching edition. With one timeline this is
// the old single-timeline behavior.
func pickTimeline(timelines [][]db.SyncTimestamp, fieldCount int) []db.SyncTimestamp {
	var best []db.SyncTimestamp
	bestDiff := -1
	for _, ts := range timelines {
		diff := len(ts) - fieldCount
		if diff < 0 {
			diff = -diff
		}
		if bestDiff < 0 || diff < bestDiff {
			best, bestDiff = ts, diff
		}
	}
	return best
}

// buildTokToFields maps each Tokenize-token index (the anchor stream's basis)
// to its whitespace-word index (sync_data's basis). Tokenize splits on every
// non-alphanumeric char while sync words are whitespace-delimited, so a word
// like "well-known" is one sync word but two Tokenize tokens. Walking the same
// content chapters by Fields and counting Tokenize sub-tokens per word yields
// the exact map. nil for the embedding path (already in Fields basis).
func buildTokToFields(chapters []ChapterText) []int {
	var m []int
	fieldsIdx := 0
	for _, ch := range chapters {
		for _, fw := range strings.Fields(ch.Text) {
			for j := 0; j < len(Tokenize(fw)); j++ {
				m = append(m, fieldsIdx)
			}
			fieldsIdx++
		}
	}
	return m
}

// bakeSegmentTimes resolves audio start/end seconds onto each aligned segment
// from the transcript timeline. tokToFields converts transcript token offsets
// to sync indices (nil = identity, Fields basis). With withWordSecs, also
// fills per-ebook-word start times (the word-karaoke path).
func bakeSegmentTimes(segs []Segment, timeline []db.SyncTimestamp, tokToFields []int, withWordSecs bool) {
	n := len(timeline)
	toSync := func(transIdx int) int {
		i := transIdx
		if tokToFields != nil {
			if i < 0 {
				i = 0
			}
			if i >= len(tokToFields) {
				i = len(tokToFields) - 1
			}
			if i < 0 {
				return -1
			}
			i = tokToFields[i]
		}
		if i < 0 {
			return 0
		}
		if i >= n {
			return n - 1
		}
		return i
	}
	for k := range segs {
		s := &segs[k]
		if s.Kind != SegAligned {
			continue
		}
		siStart, siEnd := toSync(s.TransStart), toSync(s.TransEnd-1)
		if siStart < 0 || siEnd < 0 {
			continue
		}
		s.StartSec = timeline[siStart].Start
		s.EndSec = timeline[siEnd].End
		if !withWordSecs {
			continue
		}
		ew, tw := s.EbookEnd-s.EbookStart, s.TransEnd-s.TransStart
		if ew <= 0 {
			continue
		}
		ws := make([]float64, ew)
		for e := 0; e < ew; e++ {
			tTok := s.TransStart
			if tw > 0 {
				tTok = s.TransStart + e*tw/ew
			}
			ws[e] = timeline[toSync(tTok)].Start
		}
		s.WordSecs = ws
	}
}

// MapEbookToTrans maps an ebook global word range to the corresponding
// transcript global word range using the aligned segments. Within an aligned
// segment the two sides advance together, so the offset is interpolated.
// Returns ok=false if the range falls entirely in a divergent (non-aligned)
// region. This is the structural half of "project ebook structure onto audio
// time"; compose the returned transcript range with TransChapters + sync_data
// to get the timestamp.
func MapEbookToTrans(payload AnchorAlignmentPayload, ebookStart, ebookEnd int) (transStart, transEnd int, ok bool) {
	ts, te := -1, -1
	for _, s := range payload.Segments {
		if s.Kind != SegAligned {
			continue
		}
		// overlap of [ebookStart,ebookEnd) with this aligned segment
		lo := max(ebookStart, s.EbookStart)
		hi := min(ebookEnd, s.EbookEnd)
		if lo >= hi {
			continue
		}
		espan := s.EbookEnd - s.EbookStart
		tspan := s.TransEnd - s.TransStart
		// linear interpolation within the segment
		mapPos := func(e int) int {
			if espan == 0 {
				return s.TransStart
			}
			return s.TransStart + (e-s.EbookStart)*tspan/espan
		}
		mlo, mhi := mapPos(lo), mapPos(hi)
		if ts < 0 || mlo < ts {
			ts = mlo
		}
		if mhi > te {
			te = mhi
		}
	}
	if ts < 0 {
		return 0, 0, false
	}
	return ts, te, true
}
