package llm

import (
	"fmt"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// RAG orchestrates retrieval-augmented generation for book Q&A.
type RAG struct {
	store  *db.Store
	client *Client
}

func NewRAG(store *db.Store, client *Client) *RAG {
	return &RAG{store: store, client: client}
}

// Citation references a specific location in the text.
type Citation struct {
	ChapterIdx int    `json:"chapter_idx"`
	ChapterTitle string `json:"chapter_title,omitempty"`
	StartWord  int    `json:"start_word"`
	EndWord    int    `json:"end_word"`
	Excerpt    string `json:"excerpt"`
}

// Answer is the result of a RAG query.
type Answer struct {
	Text      string     `json:"text"`
	Citations []Citation `json:"citations"`
	Model     string     `json:"model"`
	Chunks    int        `json:"chunks_used"`
}

// Ask queries a book with a question using RAG.
func (r *RAG) Ask(bookID int64, question string, bookTitle string) (*Answer, error) {
	// 1. Retrieve relevant chunks via keyword search
	// TODO: Replace with vector similarity when embeddings are available
	chunks, err := r.store.SearchChunks(bookID, extractKeywords(question))
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
