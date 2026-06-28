// Work-scoped Q&A with alignment-aware citations. Runs the RAG pipeline
// (vector search if embeddings are populated, else keyword fallback) and
// enriches each citation with the corresponding audio time range via the
// alignments table (#86) + sync_data timestamps.
package library

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

// QueryScope narrows chunk retrieval before the LLM context is built so
// the reader can ask about "just this chapter" or avoid spoilers with
// "up to here". Zero value (Type == "" or "book") = whole work, which
// preserves prior behavior for callers that don't pass a scope.
//
// BookID identifies which text book the scope applies to — required for
// any non-book scope because a work can host multiple text sources
// (e.g. EPUB + whisper transcript). ChapterIdx + ParagraphIdx are
// 0-based and only consulted when the Type requires them.
type QueryScope struct {
	Type         string `json:"type,omitempty"`
	BookID       int64  `json:"book_id,omitempty"`
	ChapterIdx   int    `json:"chapter_idx,omitempty"`
	ParagraphIdx int    `json:"paragraph_idx,omitempty"`
}

// isWholeBook is the explicit no-op test — separate from the zero
// value so a future "book" type still reads cleanly at call sites.
func (s QueryScope) isWholeBook() bool {
	return s.Type == "" || s.Type == "book"
}

// ResolveSessionScope maps a chat's per-session spoiler mode (#130) + the
// reader's live position to the effective retrieval scope:
//   - "reading" (default, spoiler-safe) → up_to_chapter at the reader's current
//     chapter, so retrieval never reaches content past where they are;
//   - "book" → the whole book (the user opted in to possible spoilers).
//
// A more-restrictive per-turn override (a paragraph/chapter the user explicitly
// pointed at on screen — necessarily at or behind their position) is honored as
// the narrower scope. With no reader position in "reading" mode, falls back to
// whole-book (the reader isn't open, so there's no position to protect).
func ResolveSessionScope(mode string, readerBookID int64, readerChapter int, override QueryScope) QueryScope {
	if override.Type == "paragraph" || override.Type == "chapter" {
		return override
	}
	if mode == "book" {
		return QueryScope{Type: "book"}
	}
	// reading (spoiler-safe) — bound to the reader's current chapter.
	if readerBookID != 0 && readerChapter >= 0 {
		return QueryScope{Type: "up_to_chapter", BookID: readerBookID, ChapterIdx: readerChapter}
	}
	return QueryScope{Type: "book"}
}

// FilterChunks returns the subset of chunks that fall inside the scope.
// Whole-book scopes are pass-through. Paragraph scope resolves the
// paragraph's word range from the paragraphs table; if that lookup
// fails the scope degrades to chapter so the user still gets *some*
// answer instead of an empty hit list.
func (s QueryScope) FilterChunks(store *db.Store, chunks []db.Chunk) []db.Chunk {
	if s.isWholeBook() || len(chunks) == 0 {
		return chunks
	}
	switch s.Type {
	case "chapter":
		return filterChunks(chunks, func(c db.Chunk) bool {
			return c.BookID == s.BookID && c.ChapterIdx == s.ChapterIdx
		})
	case "up_to_chapter":
		return filterChunks(chunks, func(c db.Chunk) bool {
			return c.BookID == s.BookID && c.ChapterIdx <= s.ChapterIdx
		})
	case "paragraph":
		paras, _ := store.ListParagraphs(s.BookID, s.ChapterIdx)
		var p *db.Paragraph
		for i := range paras {
			if paras[i].ParagraphIdx == s.ParagraphIdx {
				p = &paras[i]
				break
			}
		}
		if p == nil {
			// Degrade to chapter — the paragraph row is missing
			// (book may not be paragraph-populated yet, #91).
			return filterChunks(chunks, func(c db.Chunk) bool {
				return c.BookID == s.BookID && c.ChapterIdx == s.ChapterIdx
			})
		}
		return filterChunks(chunks, func(c db.Chunk) bool {
			if c.BookID != s.BookID || c.ChapterIdx != s.ChapterIdx {
				return false
			}
			// half-open word ranges; reject when disjoint.
			return c.EndWord > p.WordStart && c.StartWord < p.WordEnd
		})
	}
	return chunks
}

func filterChunks(in []db.Chunk, keep func(db.Chunk) bool) []db.Chunk {
	out := in[:0:0]
	for _, c := range in {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// fetchChunksInScope returns every chunk inside the scope without
// vector/keyword ranking — used as a fallback when retrieval comes
// back empty after scoping, and as the primary path for paragraph
// scope where the candidate set is already small enough for the LLM
// to digest directly.
func fetchChunksInScope(store *db.Store, scope QueryScope) []db.Chunk {
	if scope.isWholeBook() || scope.BookID == 0 {
		return nil
	}
	all, _ := store.ListChunks(scope.BookID)
	return scope.FilterChunks(store, all)
}

// AskWithCitations answers a question against a work using all its text
// sources, preferring vector search when embeddings exist. Citations
// include audio time ranges when forced alignment is available.
//
// scope narrows retrieval to a single chapter, "up to here" (spoiler
// avoidance), or a paragraph. Pass the zero value for whole-work.
func AskWithCitations(store *db.Store, rag *llm.RAG, workID int64, question string, scope QueryScope) (*llm.Answer, error) {
	if rag == nil || rag.Client() == nil {
		return nil, fmt.Errorf("LLM not configured")
	}
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return nil, fmt.Errorf("work not found")
	}

	// Pick the highest-authority text book for retrieval.
	target := ResolveAlignmentTarget(work)
	if target == nil {
		return nil, fmt.Errorf("no text content for this work")
	}

	retrieved, err := retrievePassages(store, rag, work, target, question, scope)
	if err != nil {
		return nil, err
	}
	if len(retrieved) == 0 {
		return &llm.Answer{
			Text:   "I couldn't find any relevant passages in the book to answer that question.",
			Chunks: 0,
		}, nil
	}

	// Load alignment data so we can attach audio timestamps to citations.
	ac := newAlignmentContext(store, workID)

	// Chapter titles per book for citation display.
	titleCache := map[int64]map[int]string{}
	getTitle := func(bookID int64, ch int) string {
		m, ok := titleCache[bookID]
		if !ok {
			m = map[int]string{}
			chapters, _ := store.ListChapters(bookID)
			for _, c := range chapters {
				m[c.Index] = c.Title
			}
			titleCache[bookID] = m
		}
		if t, ok := m[ch]; ok && t != "" {
			return t
		}
		return fmt.Sprintf("Chapter %d", ch+1)
	}

	// Build context string + citations.
	var contextBuf strings.Builder
	var citations []llm.Citation
	for i, chunk := range retrieved {
		chTitle := getTitle(chunk.BookID, chunk.ChapterIdx)
		contextBuf.WriteString(fmt.Sprintf("[Passage %d - %s]\n", i+1, chTitle))
		contextBuf.WriteString(chunk.Content)
		contextBuf.WriteString("\n\n")

		excerpt := chunk.Content
		if len(excerpt) > 150 {
			excerpt = excerpt[:150] + "..."
		}
		cit := llm.Citation{
			BookID:       chunk.BookID,
			ChapterIdx:   chunk.ChapterIdx,
			ChapterTitle: chTitle,
			StartWord:    chunk.StartWord,
			EndWord:      chunk.EndWord,
			Excerpt:      excerpt,
		}
		// Attempt to attach audio timestamps via alignment.
		if abkID, startSec, endSec, ok := ac.audioTimesFor(chunk); ok {
			cit.AudioStartSec = startSec
			cit.AudioEndSec = endSec
			cit.AudioBookID = abkID
		}
		citations = append(citations, cit)
	}

	// Spoiler-safety (#130). The retrieval above is already bounded to what the
	// reader has read (citations never exceed the scope), which is the
	// ENFORCEABLE guarantee. For a bounded scope we also swap to a strict
	// extraction prompt that omits the book title (the title alone primes a
	// model's recall) and forbids outside knowledge — strong for books the
	// model hasn't memorized. NOTE: for a few ultra-famous public-domain
	// classics that the model has memorized verbatim, generation can still draw
	// on that training knowledge despite the prompt; that residual leak is a
	// known LLM limitation, not a retrieval-scope failure.
	citationStyle := `Cite by chapter name or a short inline quote (e.g., 'In Chapter 5, the author argues…'); NEVER mention "Passage N" — the reader sees a separate Sources panel, not passage numbers.`
	var systemPrompt, userMessage string
	if scope.isWholeBook() {
		systemPrompt = fmt.Sprintf(`You are a knowledgeable literary assistant helping a reader understand "%s".
Answer questions using ONLY the provided passages from the book — never outside knowledge.
%s
If the passages don't contain enough information to answer, say so honestly.
Keep answers concise but thorough — 2-4 paragraphs.`, work.Title, citationStyle)
		userMessage = fmt.Sprintf("Here are relevant passages from the book:\n\n%s\n\nQuestion: %s",
			contextBuf.String(), question)
	} else {
		systemPrompt = fmt.Sprintf(`You answer a reader's question using ONLY the passages provided — the only part of the book they have read so far. You do NOT know this book; ignore anything you might recall about it from elsewhere and rely solely on these passages.

Hard rules:
- Use ONLY facts explicitly stated in the passages. No outside knowledge, no guessing, no recognizing the book.
- If the passages do not clearly contain the answer, reply with EXACTLY this and nothing more: "That hasn't come up yet in what you've read."
- Never name or describe a character, event, death, twist, or ending that is not present in the passages — that would spoil the story for the reader.
%s`, citationStyle)
		userMessage = fmt.Sprintf("Passages the reader has read:\n\n%s\n\nUsing only these passages, answer: %s\n\n(If it isn't in the passages, reply exactly: \"That hasn't come up yet in what you've read.\")",
			contextBuf.String(), question)
	}

	resp, err := rag.Client().Complete(llm.CompletionRequest{
		System: systemPrompt,
		Messages: []llm.Message{
			{Role: "user", Content: userMessage},
		},
		MaxTokens:   1024,
		Temperature: 0.3,
	})
	if err != nil {
		return nil, fmt.Errorf("llm completion: %w", err)
	}

	return &llm.Answer{
		Text:      resp.Content,
		Citations: citations,
		Model:     resp.Model,
		Chunks:    len(retrieved),
	}, nil
}

// retrievePassages runs the vector → keyword → chapter-fallback ladder
// shared by AskWithCitations and AskInSession. Scoping is applied
// after retrieval so the ranker sees the full embedding space; if
// scoping leaves zero candidates we backstop by loading every chunk
// inside the scope so a narrow ask never returns empty.
//
// Paragraph scope skips the ranker entirely — the candidate set is
// already small, and the user explicitly told us where they're
// pointing.
func retrievePassages(store *db.Store, rag *llm.RAG, work *db.Work, target *db.Book, question string, scope QueryScope) ([]db.Chunk, error) {
	if scope.Type == "paragraph" {
		return fetchChunksInScope(store, scope), nil
	}

	// Widen vector topK when scoping so post-filter still has room.
	topK := 8
	if !scope.isWholeBook() {
		topK = 24
	}

	var retrieved []db.Chunk
	hits, err := VectorSearchChunks(store, rag.Client(), work.ID, question, topK)
	if err == nil && len(hits) > 0 {
		for _, h := range hits {
			retrieved = append(retrieved, h.Chunk)
		}
		log.Printf("qa: vector search returned %d chunks for work %d (scope=%q)",
			len(retrieved), work.ID, scope.Type)
	} else {
		// Keyword fallback. When scope names a book, search that book
		// instead of the auto-resolved target so a transcript-vs-EPUB
		// pick can't override the user's explicit choice.
		searchBookID := target.ID
		if scope.BookID > 0 {
			searchBookID = scope.BookID
		}
		kw := extractKeyword(question)
		retrieved, _ = store.SearchChunks(searchBookID, kw)
		log.Printf("qa: keyword fallback returned %d chunks for work %d (scope=%q, query=%q)",
			len(retrieved), work.ID, scope.Type, kw)
	}

	retrieved = scope.FilterChunks(store, retrieved)

	// Empty after filter on a narrow scope → fall back to the in-scope
	// chunk list. The user picked a specific chapter; better to feed
	// the LLM the whole chapter than to claim there's no answer.
	if len(retrieved) == 0 && !scope.isWholeBook() {
		retrieved = fetchChunksInScope(store, scope)
	}

	const limit = 8
	if len(retrieved) > limit {
		retrieved = retrieved[:limit]
	}
	return retrieved, nil
}

// extractKeyword pulls the most specific (longest) non-stopword from a
// question for keyword fallback search.
func extractKeyword(question string) string {
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "what": true, "who": true, "where": true,
		"when": true, "why": true, "how": true, "does": true, "did": true,
		"do": true, "has": true, "have": true, "had": true, "this": true,
		"that": true, "these": true, "those": true, "it": true, "its": true,
		"in": true, "on": true, "at": true, "to": true, "for": true,
		"of": true, "with": true, "by": true, "from": true, "about": true,
	}
	var best string
	for _, word := range strings.Fields(strings.ToLower(question)) {
		word = strings.Trim(word, "?.,!;:'\"")
		if len(word) > 2 && !stopWords[word] && len(word) > len(best) {
			best = word
		}
	}
	if best == "" {
		return question
	}
	return best
}

// alignmentContext caches parsed alignments + sync data so citation
// enrichment doesn't re-parse per chunk.
type alignmentContext struct {
	store         *db.Store
	workID        int64
	// byEbookBook: ebook_book_id → list of parsed alignment pairs, plus the
	// transcript book id those pairs map to.
	byEbookBook map[int64]*ebookAlignmentData
	// syncByChapter: (audio_book_id, chapter_idx) → transcript word timestamps
	syncByKey map[string][]db.SyncTimestamp
	// audioBookForWork: cached first audio book ID for the work (used when
	// we don't know which specific audio file a transcript corresponds to)
	audioBookID int64
}

type ebookAlignmentData struct {
	transcriptBookID int64
	pairs            []db.AlignmentPair       // edit-distance method
	anchor           *AnchorAlignmentPayload  // anchor/embedding (render-ready, preferred)
}

func newAlignmentContext(store *db.Store, workID int64) *alignmentContext {
	ac := &alignmentContext{
		store:       store,
		workID:      workID,
		byEbookBook: map[int64]*ebookAlignmentData{},
		syncByKey:   map[string][]db.SyncTimestamp{},
	}
	work, _ := store.GetWork(workID)
	if work != nil && len(work.AudioFiles) > 0 {
		ac.audioBookID = work.AudioFiles[0].ID
	}
	// Preload alignments. Anchor/embedding payloads are render-ready (audio
	// times baked) so we prefer them over edit-distance when both exist for
	// the same ebook.
	alignments, _ := store.ListAlignmentsForWork(workID)
	for _, a := range alignments {
		switch a.Method {
		case "edit-distance":
			var pairs []db.AlignmentPair
			if err := json.Unmarshal([]byte(a.Pairs), &pairs); err != nil {
				continue
			}
			d := ac.byEbookBook[a.FromBookID]
			if d == nil {
				d = &ebookAlignmentData{transcriptBookID: a.ToBookID}
				ac.byEbookBook[a.FromBookID] = d
			}
			d.pairs = pairs
		case "anchor", "embedding":
			var payload AnchorAlignmentPayload
			if err := json.Unmarshal([]byte(a.Pairs), &payload); err != nil {
				continue
			}
			d := ac.byEbookBook[a.FromBookID]
			if d == nil {
				d = &ebookAlignmentData{transcriptBookID: a.ToBookID}
				ac.byEbookBook[a.FromBookID] = d
			}
			d.anchor = &payload
		}
	}
	return ac
}

// audioTimesFor resolves audio start/end seconds for a chunk. Returns
// (audioBookID, startSec, endSec, ok=true) if alignment + sync data
// successfully compose the chunk's time range.
func (ac *alignmentContext) audioTimesFor(chunk db.Chunk) (int64, float64, float64, bool) {
	// Case 1: chunk is in a transcript book — sync_data is keyed directly.
	//   (book_id=transcriptID, chapter_idx=N). We need audioBookID + times.
	// Case 2: chunk is in an ebook — use alignment to find transcript words,
	//   then look up sync_data for those transcript words.

	// Try Case 1 first: is this chunk's book a transcript? We can detect by
	// checking whether we have sync_data for (workID, audioBookID, chapterIdx)
	// with the chapter index the chunk references — but we'd need to know
	// which audio book. For single-file works there's only one.
	if ac.audioBookID != 0 {
		if ts := ac.loadSync(ac.audioBookID, chunk.ChapterIdx); ts != nil {
			// Check if the chunk's word range is within the transcript size
			// (meaning this chunk IS the transcript).
			if chunk.StartWord < len(ts) {
				startSec := ts[chunk.StartWord].Start
				endIdx := chunk.EndWord - 1
				if endIdx >= len(ts) {
					endIdx = len(ts) - 1
				}
				endSec := ts[endIdx].End
				return ac.audioBookID, startSec, endSec, true
			}
		}
	}

	// Case 2: chunk is in an ebook — use alignment to map ebook word range
	// to audio time. Anchor/embedding payloads have baked audio times; the
	// edit-distance fallback composes pairs with sync_data.
	aln, ok := ac.byEbookBook[chunk.BookID]
	if !ok {
		return 0, 0, 0, false
	}
	if aln.anchor != nil {
		if abID, s, e, ok := ac.anchorTimesFor(chunk, aln.anchor); ok {
			return abID, s, e, true
		}
	}
	// Find alignment pair(s) covering this chunk's word range.
	var tStart, tEnd int = -1, -1
	for _, p := range aln.pairs {
		if p.FromChapter != chunk.ChapterIdx {
			continue
		}
		// Check overlap with [chunk.StartWord, chunk.EndWord)
		if p.FromEnd <= chunk.StartWord || p.FromStart >= chunk.EndWord {
			continue
		}
		if tStart < 0 || p.ToStart < tStart {
			tStart = p.ToStart
		}
		if p.ToEnd > tEnd {
			tEnd = p.ToEnd
		}
	}
	if tStart < 0 {
		return 0, 0, 0, false
	}
	// Now look up transcript timestamps for this chapter.
	ts := ac.loadSync(ac.audioBookID, chunk.ChapterIdx)
	if ts == nil || tStart >= len(ts) {
		return 0, 0, 0, false
	}
	endIdx := tEnd - 1
	if endIdx >= len(ts) {
		endIdx = len(ts) - 1
	}
	if endIdx < tStart {
		endIdx = tStart
	}
	return ac.audioBookID, ts[tStart].Start, ts[endIdx].End, true
}

// anchorTimesFor resolves audio times for an ebook chunk using a render-ready
// AnchorAlignmentPayload. Maps the chunk's chapter-local word range to global
// ebook offsets via EbookChapters, then finds overlapping aligned segments and
// reads the baked audio times directly (no sync_data needed).
func (ac *alignmentContext) anchorTimesFor(chunk db.Chunk, p *AnchorAlignmentPayload) (int64, float64, float64, bool) {
	var base, length int = -1, 0
	for _, cs := range p.EbookChapters {
		if cs.Index == chunk.ChapterIdx {
			base = cs.Start
			length = cs.Len
			break
		}
	}
	if base < 0 {
		return 0, 0, 0, false
	}
	lo := chunk.StartWord
	hi := chunk.EndWord
	if lo < 0 {
		lo = 0
	}
	if hi > length {
		hi = length
	}
	if hi <= lo {
		return 0, 0, 0, false
	}
	gStart := base + lo
	gEnd := base + hi

	var startSec, endSec float64 = -1, -1
	for _, s := range p.Segments {
		if s.Kind != SegAligned {
			continue
		}
		segLo := max(gStart, s.EbookStart)
		segHi := min(gEnd, s.EbookEnd)
		if segLo >= segHi {
			continue
		}
		var sStart, sEnd float64
		if len(s.WordSecs) > 0 {
			// Word path: WordSecs[i] = start of the (EbookStart+i)-th ebook word.
			relStart := segLo - s.EbookStart
			relEnd := segHi - s.EbookStart // half-open
			if relStart < 0 {
				relStart = 0
			}
			if relStart >= len(s.WordSecs) {
				relStart = len(s.WordSecs) - 1
			}
			sStart = s.WordSecs[relStart]
			if relEnd >= len(s.WordSecs) {
				sEnd = s.EndSec
			} else {
				sEnd = s.WordSecs[relEnd]
			}
		} else {
			// Paragraph path: interpolate within the segment's time range.
			span := s.EbookEnd - s.EbookStart
			if span <= 0 {
				sStart, sEnd = s.StartSec, s.EndSec
			} else {
				dur := s.EndSec - s.StartSec
				sStart = s.StartSec + dur*float64(segLo-s.EbookStart)/float64(span)
				sEnd = s.StartSec + dur*float64(segHi-s.EbookStart)/float64(span)
			}
		}
		if startSec < 0 || sStart < startSec {
			startSec = sStart
		}
		if sEnd > endSec {
			endSec = sEnd
		}
	}
	if startSec < 0 {
		return 0, 0, 0, false
	}
	return ac.audioBookID, startSec, endSec, true
}

// loadSync fetches (and caches) sync_data for an (audioBookID, chapterIdx).
func (ac *alignmentContext) loadSync(audioBookID int64, chapterIdx int) []db.SyncTimestamp {
	key := fmt.Sprintf("%d:%d", audioBookID, chapterIdx)
	if ts, ok := ac.syncByKey[key]; ok {
		return ts
	}
	raw, _ := ac.store.GetSyncData(ac.workID, audioBookID, chapterIdx)
	if raw == "" {
		ac.syncByKey[key] = nil
		return nil
	}
	var ts []db.SyncTimestamp
	if err := json.Unmarshal([]byte(raw), &ts); err != nil {
		ac.syncByKey[key] = nil
		return nil
	}
	ac.syncByKey[key] = ts
	return ts
}
