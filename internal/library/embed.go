// Compute and store vector embeddings for text chunks, enabling semantic
// search for RAG Q&A. Requires an OpenAI API key (BYOK).
//
// Chunks without embeddings get embedded in batches of up to 100 texts per
// API call. Results are stored as raw float32 bytes in the chunks.embedding
// BLOB column. Cosine similarity is computed in pure Go.
package library

import (
	"log"
	"sort"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

const embedBatchSize = 100 // OpenAI allows up to 2048 inputs per call

// EmbedChunksForBook computes embeddings for any chunk of a book that doesn't
// have one yet. Returns the number of chunks newly embedded.
func EmbedChunksForBook(store *db.Store, client *llm.Client, bookID int64) (int, error) {
	chunks, err := store.ListChunks(bookID)
	if err != nil {
		return 0, err
	}

	// Filter to un-embedded chunks.
	var todo []db.Chunk
	for _, c := range chunks {
		if len(c.Embedding) == 0 {
			todo = append(todo, c)
		}
	}
	if len(todo) == 0 {
		return 0, nil
	}

	log.Printf("embed: %d chunks need embeddings for book %d", len(todo), bookID)

	embedded := 0
	for i := 0; i < len(todo); i += embedBatchSize {
		end := i + embedBatchSize
		if end > len(todo) {
			end = len(todo)
		}
		batch := todo[i:end]

		texts := make([]string, len(batch))
		for j, c := range batch {
			texts[j] = c.Content
		}

		resp, err := client.Embed(llm.EmbedRequest{Texts: texts})
		if err != nil {
			return embedded, err
		}

		for j, emb := range resp.Embeddings {
			if len(emb) == 0 {
				continue
			}
			blob := llm.EncodeEmbedding(emb)
			if err := store.UpdateChunkEmbedding(batch[j].ID, blob); err != nil {
				return embedded, err
			}
			embedded++
		}

		log.Printf("embed: batch %d-%d done (%d tokens)", i, end, resp.Usage)
	}

	log.Printf("embed: embedded %d chunks for book %d", embedded, bookID)
	return embedded, nil
}

// VectorSearchResult pairs a chunk with its similarity score.
type VectorSearchResult struct {
	Chunk      db.Chunk
	Similarity float64
}

// VectorSearchChunks embeds the query and returns the top-K most similar
// chunks from the work's books. Falls back to empty result if embeddings
// aren't populated or the client can't embed.
func VectorSearchChunks(store *db.Store, client *llm.Client, workID int64, query string, topK int) ([]VectorSearchResult, error) {
	if topK <= 0 {
		topK = 5
	}

	// Embed the query.
	resp, err := client.Embed(llm.EmbedRequest{Texts: []string{query}})
	if err != nil || len(resp.Embeddings) == 0 || len(resp.Embeddings[0]) == 0 {
		return nil, err
	}
	queryVec := resp.Embeddings[0]

	// Load all embedded chunks for this work.
	chunks, err := store.ListAllChunksWithEmbeddings(workID)
	if err != nil || len(chunks) == 0 {
		return nil, err
	}

	// Score each chunk.
	results := make([]VectorSearchResult, 0, len(chunks))
	for _, c := range chunks {
		chunkVec := llm.DecodeEmbedding(c.Embedding)
		if len(chunkVec) == 0 {
			continue
		}
		sim := llm.CosineSimilarity(queryVec, chunkVec)
		results = append(results, VectorSearchResult{Chunk: c, Similarity: sim})
	}

	// Sort descending by similarity.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})

	if len(results) > topK {
		results = results[:topK]
	}

	return results, nil
}
