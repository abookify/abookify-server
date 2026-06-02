package llm

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/pj/abookify/internal/db"
)

// RAG orchestrates retrieval-augmented generation for book Q&A.
type RAG struct {
	store  *db.Store
	client *Client
}

// EmbedStore is the write surface saveEmbeddingWithRetry needs — just the
// embedding upsert. *db.Store satisfies it; a fake drives the retry test.
type EmbedStore interface {
	UpdateChunkEmbedding(chunkID int64, embedding []byte) error
}

func NewRAG(store *db.Store, client *Client) *RAG {
	return &RAG{store: store, client: client}
}

// Client returns the underlying LLM client (used for embeddings).
func (r *RAG) Client() *Client {
	return r.client
}

// Citation references a specific location in the text — and, when
// forced alignment is available, the corresponding audio time range.
type Citation struct {
	BookID       int64   `json:"book_id"`
	ChapterIdx   int     `json:"chapter_idx"`
	ChapterTitle string  `json:"chapter_title,omitempty"`
	StartWord    int     `json:"start_word"`
	EndWord      int     `json:"end_word"`
	// Audio time range in the aligned audio (via alignments table).
	// Zero if no alignment is available for this chunk's book.
	AudioStartSec float64 `json:"audio_start_sec,omitempty"`
	AudioEndSec   float64 `json:"audio_end_sec,omitempty"`
	AudioBookID   int64   `json:"audio_book_id,omitempty"`
	Excerpt      string   `json:"excerpt"`
}

// Answer is the result of a RAG query.
type Answer struct {
	Text      string     `json:"text"`
	Citations []Citation `json:"citations"`
	Model     string     `json:"model"`
	Chunks    int        `json:"chunks_used"`
}

// EmbedBook backfills embeddings for every chunk in a book that doesn't
// already have one. Returns count of newly-embedded chunks. Idempotent.
// saveEmbeddingWithRetry persists one chunk's embedding, retrying through
// brief SQLITE_BUSY/LOCKED windows (#207). busy_timeout covers most
// contention, but a long concurrent writer during a multi-thousand-chunk
// backfill can still trip a discrete write; without this, one BUSY aborts the
// whole run mid-batch and loses progress. Exponential backoff: 50→800ms, 5
// tries. Non-busy errors return immediately.
func saveEmbeddingWithRetry(store EmbedStore, chunkID int64, blob []byte) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = store.UpdateChunkEmbedding(chunkID, blob); err == nil || !db.IsBusyErr(err) {
			return err
		}
		time.Sleep(time.Duration(50*(1<<attempt)) * time.Millisecond)
	}
	return err
}

func (r *RAG) EmbedBook(bookID int64) (int, error) {
	chunks, err := r.store.ListChunks(bookID)
	if err != nil {
		return 0, fmt.Errorf("list chunks: %w", err)
	}
	var todo []db.Chunk
	for _, c := range chunks {
		if len(c.Embedding) == 0 {
			todo = append(todo, c)
		}
	}
	if len(todo) == 0 {
		return 0, nil
	}

	log.Printf("rag: embedding %d chunks for book %d (provider: %s)", len(todo), bookID, r.client.provider)

	// Batch the texts. OpenAI handles big batches natively; Ollama loops
	// internally one prompt at a time. We send 32 at a time so a Ctrl+C
	// doesn't lose more than ~1% of a 3000-chunk run.
	const batch = 32
	embedded := 0
	for i := 0; i < len(todo); i += batch {
		j := i + batch
		if j > len(todo) {
			j = len(todo)
		}
		texts := make([]string, 0, j-i)
		for _, c := range todo[i:j] {
			texts = append(texts, c.Content)
		}
		resp, err := r.client.Embed(EmbedRequest{Texts: texts})
		if err != nil {
			return embedded, fmt.Errorf("embed batch %d-%d: %w", i, j, err)
		}
		for k, vec := range resp.Embeddings {
			if len(vec) == 0 {
				continue
			}
			if err := saveEmbeddingWithRetry(r.store, todo[i+k].ID, EncodeEmbedding(vec)); err != nil {
				return embedded, fmt.Errorf("save embedding %d: %w", todo[i+k].ID, err)
			}
			embedded++
		}
		log.Printf("rag: embedded %d/%d chunks (book %d)", embedded, len(todo), bookID)
	}
	return embedded, nil
}

// vectorRetrieve returns the top-K chunks for a question by cosine
// similarity over the stored embeddings. Returns nil if vector search
// isn't possible (no embeddings, no embed support, etc.) so the caller
// can fall back to keyword search.
func (r *RAG) vectorRetrieve(bookID int64, question string, topK int) []db.Chunk {
	all, err := r.store.ListChunks(bookID)
	if err != nil || len(all) == 0 {
		return nil
	}
	// Quick check: only proceed if at least one chunk has an embedding.
	any := false
	for _, c := range all {
		if len(c.Embedding) > 0 {
			any = true
			break
		}
	}
	if !any {
		return nil
	}

	qResp, err := r.client.Embed(EmbedRequest{Texts: []string{question}})
	if err != nil || len(qResp.Embeddings) == 0 || len(qResp.Embeddings[0]) == 0 {
		return nil
	}
	qVec := qResp.Embeddings[0]

	type scored struct {
		c   db.Chunk
		sim float64
	}
	var ranked []scored
	for _, c := range all {
		if len(c.Embedding) == 0 {
			continue
		}
		vec := DecodeEmbedding(c.Embedding)
		if len(vec) != len(qVec) {
			// Different embed model used at backfill time vs now — skip.
			continue
		}
		ranked = append(ranked, scored{c: c, sim: CosineSimilarity(qVec, vec)})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].sim > ranked[j].sim })
	if len(ranked) > topK {
		ranked = ranked[:topK]
	}
	out := make([]db.Chunk, len(ranked))
	for i, s := range ranked {
		out[i] = s.c
	}
	return out
}

// Ask queries a book with a question using RAG. Tries vector retrieval
// first (when embeddings exist); falls back to keyword search.
func (r *RAG) Ask(bookID int64, question string, bookTitle string) (*Answer, error) {
	// 1. Retrieve relevant chunks. Prefer vector similarity if embeddings
	// are available; fall back to keyword search otherwise.
	chunks := r.vectorRetrieve(bookID, question, 8)
	if chunks != nil {
		log.Printf("qa: vector retrieved %d chunks for book %d (query: %q)", len(chunks), bookID, truncate(question, 60))
	} else {
		log.Printf("qa: no embeddings for book %d, using keyword fallback", bookID)
		var err error
		chunks, err = r.store.SearchChunks(bookID, extractKeywords(question))
		if err != nil {
			return nil, fmt.Errorf("search chunks: %w", err)
		}
		if len(chunks) == 0 {
			// Try with individual words
			for _, word := range strings.Fields(question) {
				if len(word) > 3 {
					more, _ := r.store.SearchChunks(bookID, word)
					chunks = append(chunks, more...)
					if len(chunks) >= 10 {
						break
					}
				}
			}
		}
	}

	if len(chunks) == 0 {
		return &Answer{
			Text:   "I couldn't find any relevant passages in the book to answer that question.",
			Chunks: 0,
		}, nil
	}

	// Deduplicate and limit to top 8 chunks
	seen := map[string]bool{}
	var unique []db.Chunk
	for _, c := range chunks {
		key := fmt.Sprintf("%d-%d", c.ChapterIdx, c.ChunkIdx)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, c)
		}
		if len(unique) >= 8 {
			break
		}
	}

	// 2. Get chapter titles for citations
	chapters, _ := r.store.ListChapters(bookID)
	chapterTitles := map[int]string{}
	for _, ch := range chapters {
		chapterTitles[ch.Index] = ch.Title
	}

	// 3. Build context from chunks
	var context strings.Builder
	var citations []Citation

	for i, chunk := range unique {
		chTitle := chapterTitles[chunk.ChapterIdx]
		if chTitle == "" {
			chTitle = fmt.Sprintf("Chapter %d", chunk.ChapterIdx)
		}

		context.WriteString(fmt.Sprintf("[Passage %d - %s, words %d-%d]\n", i+1, chTitle, chunk.StartWord, chunk.EndWord))
		context.WriteString(chunk.Content)
		context.WriteString("\n\n")

		// Prepare citation with excerpt
		excerpt := chunk.Content
		if len(excerpt) > 150 {
			excerpt = excerpt[:150] + "..."
		}
		citations = append(citations, Citation{
			ChapterIdx:   chunk.ChapterIdx,
			ChapterTitle: chTitle,
			StartWord:    chunk.StartWord,
			EndWord:      chunk.EndWord,
			Excerpt:      excerpt,
		})
	}

	// 4. Build the prompt
	systemPrompt := fmt.Sprintf(`You are a knowledgeable literary assistant helping a reader understand "%s".
Answer questions based ONLY on the provided passages from the book.
Be specific and reference which passages support your answer (e.g., "In Passage 3...").
If the passages don't contain enough information to answer, say so honestly.
Keep answers concise but thorough — 2-4 paragraphs.`, bookTitle)

	userMessage := fmt.Sprintf("Here are relevant passages from the book:\n\n%s\n\nQuestion: %s", context.String(), question)

	// 5. Query the LLM
	resp, err := r.client.Complete(CompletionRequest{
		System: systemPrompt,
		Messages: []Message{
			{Role: "user", Content: userMessage},
		},
		MaxTokens:   1024,
		Temperature: 0.3,
	})
	if err != nil {
		return nil, fmt.Errorf("llm completion: %w", err)
	}

	return &Answer{
		Text:      resp.Content,
		Citations: citations,
		Model:     resp.Model,
		Chunks:    len(unique),
	}, nil
}

// truncate cuts a string to n characters with an ellipsis. Logging helper.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// extractKeywords pulls significant words from a question for search.
func extractKeywords(question string) string {
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "what": true, "who": true, "where": true,
		"when": true, "why": true, "how": true, "does": true, "did": true,
		"do": true, "has": true, "have": true, "had": true, "this": true,
		"that": true, "these": true, "those": true, "it": true, "its": true,
		"in": true, "on": true, "at": true, "to": true, "for": true,
		"of": true, "with": true, "by": true, "from": true, "about": true,
		"can": true, "could": true, "would": true, "should": true,
		"and": true, "or": true, "but": true, "not": true, "be": true,
	}

	var keywords []string
	for _, word := range strings.Fields(strings.ToLower(question)) {
		// Strip punctuation
		word = strings.Trim(word, "?.,!;:'\"")
		if len(word) > 2 && !stopWords[word] {
			keywords = append(keywords, word)
		}
	}

	if len(keywords) == 0 {
		return question
	}

	// Return most specific keyword (longest)
	best := keywords[0]
	for _, k := range keywords[1:] {
		if len(k) > len(best) {
			best = k
		}
	}
	return best
}
