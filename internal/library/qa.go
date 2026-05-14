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

// AskWithCitations answers a question against a work using all its text
// sources, preferring vector search when embeddings exist. Citations
// include audio time ranges when forced alignment is available.
func AskWithCitations(store *db.Store, rag *llm.RAG, workID int64, question string) (*llm.Answer, error) {
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

	// Try vector search first (requires embeddings).
	var retrieved []db.Chunk
	hits, err := VectorSearchChunks(store, rag.Client(), workID, question, 8)
	if err == nil && len(hits) > 0 {
		for _, h := range hits {
			retrieved = append(retrieved, h.Chunk)
		}
		log.Printf("qa: vector search returned %d chunks for work %d", len(retrieved), workID)
	} else {
		// Fallback: keyword search on the target book.
		kw := extractKeyword(question)
		retrieved, _ = store.SearchChunks(target.ID, kw)
		if len(retrieved) > 8 {
			retrieved = retrieved[:8]
		}
		log.Printf("qa: keyword fallback returned %d chunks for work %d (query: %q)",
			len(retrieved), workID, kw)
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

	// Invoke the underlying RAG completion via its public Client path.
	systemPrompt := fmt.Sprintf(`You are a knowledgeable literary assistant helping a reader understand "%s".
Answer questions based ONLY on the provided passages from the book.

IMPORTANT — citation style: NEVER mention "Passage N" or "Passages 3-5" or
any reference to internal passage numbers. The user does NOT see passage
numbers; they see your prose answer plus a separate Sources panel below it
that names the chapters. Cite by chapter name or by a short inline quote
(e.g., 'In Chapter 5, the author argues…' or 'as the text puts it, "…"').
The passage-N labels in your context are an internal hint for you only.

If the passages don't contain enough information to answer, say so honestly.
Keep answers concise but thorough — 2-4 paragraphs.`, work.Title)

	userMessage := fmt.Sprintf("Here are relevant passages from the book:\n\n%s\n\nQuestion: %s",
		contextBuf.String(), question)

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
	pairs            []db.AlignmentPair
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
	// Preload alignments.
	alignments, _ := store.ListAlignmentsForWork(workID)
	for _, a := range alignments {
		if a.Method != "edit-distance" {
			continue
		}
		var pairs []db.AlignmentPair
		if err := json.Unmarshal([]byte(a.Pairs), &pairs); err != nil {
			continue
		}
		ac.byEbookBook[a.FromBookID] = &ebookAlignmentData{
			transcriptBookID: a.ToBookID,
			pairs:            pairs,
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
	// to transcript word range, then transcript to audio time.
	aln, ok := ac.byEbookBook[chunk.BookID]
	if !ok {
		return 0, 0, 0, false
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
