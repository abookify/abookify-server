package library

import (
	"log"
	"strings"

	"github.com/pj/abookify/internal/db"
)

const (
	// Target chunk size in words
	chunkSize = 200
	// Overlap between chunks in words
	chunkOverlap = 40
)

// ChunkBook breaks all chapters of a book into overlapping text chunks.
func ChunkBook(store *db.Store, bookID int64) error {
	// Skip if already chunked
	count, _ := store.ChunkCount(bookID)
	if count > 0 {
		return nil
	}

	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return err
	}

	totalChunks := 0

	for _, chMeta := range chapters {
		// Load full content
		ch, err := store.GetChapterContent(bookID, chMeta.Index)
		if err != nil || ch == nil {
			continue
		}

		words := strings.Fields(ch.Content)
		if len(words) == 0 {
			continue
		}

		chunkIdx := 0
		start := 0

		for start < len(words) {
			end := start + chunkSize
			if end > len(words) {
				end = len(words)
			}

			chunkText := strings.Join(words[start:end], " ")

			chunk := db.Chunk{
				BookID:     bookID,
				ChapterIdx: ch.Index,
				ChunkIdx:   chunkIdx,
				Content:    chunkText,
				StartWord:  start,
				EndWord:    end,
			}

			if err := store.InsertChunk(chunk); err != nil {
				return err
			}

			totalChunks++
			chunkIdx++

			// Advance by (chunkSize - overlap), so chunks overlap
			start += chunkSize - chunkOverlap
		}
	}

	if totalChunks > 0 {
		log.Printf("chunked book %d into %d chunks (%d-word windows, %d-word overlap)",
			bookID, totalChunks, chunkSize, chunkOverlap)
	}

	return nil
}
