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

// chunksStale reports whether a book's existing chunks still describe its
// current chapters. Two independent signals, both exact enough to avoid
// re-embedding a book that has not actually changed:
//
//  1. The set of chapter indices carrying chunks differs from the set of
//     chapters carrying text. This is what a re-split produces — the chapter
//     count itself moves, so chapter_idx no longer means what it did.
//
//  2. A chapter's chunks stop well short of its text. This is what a
//     re-transcription produces — the chapter count is unchanged but the words
//     behind it grew (a repaired source file recovering lost narration, say).
//     Compared against the chapter's own word_count with a 10% tolerance, so
//     tokenizer drift between the counter and strings.Fields cannot put a book
//     into a permanent rebuild loop.
//
//  3. A chapter's chunks run well PAST its text. Same 10% tolerance, opposite
//     direction — and it is the signature of a repair rather than a re-split.
//     Removing fabricated words SHRINKS a chapter, so the old chunks stay
//     "long enough" under signal 2 alone and the book reads as up to date while
//     every chunk still holds invented text. Free Will proved it: the reader
//     showed the repaired words while Q&A went on citing a sentence the narrator
//     never said, because 528 real words could not make 591 stale ones look short.
func chunksStale(store *db.Store, bookID int64, chapters []db.Chapter) (bool, error) {
	chunks, err := store.ListChunks(bookID)
	if err != nil {
		return false, err
	}

	chunkedIdx := map[int]bool{}
	maxEnd := map[int]int{}
	for _, c := range chunks {
		chunkedIdx[c.ChapterIdx] = true
		if c.EndWord > maxEnd[c.ChapterIdx] {
			maxEnd[c.ChapterIdx] = c.EndWord
		}
	}

	textIdx := map[int]bool{}
	for _, ch := range chapters {
		if ch.WordCount > 0 {
			textIdx[ch.Index] = true
		}
	}

	if len(chunkedIdx) != len(textIdx) {
		return true, nil
	}
	for idx := range textIdx {
		if !chunkedIdx[idx] {
			return true, nil
		}
	}
	for _, ch := range chapters {
		if ch.WordCount == 0 {
			continue
		}
		if float64(maxEnd[ch.Index]) < float64(ch.WordCount)*0.9 {
			return true, nil // text grew past the chunks
		}
		if float64(maxEnd[ch.Index]) > float64(ch.WordCount)*1.1 {
			return true, nil // chunks outlive the text: a repair removed words
		}
	}
	return false, nil
}

// ChunkBook breaks all chapters of a book into overlapping text chunks.
//
// Chunks are rebuilt when they no longer describe the book's current chapters.
// A transcript gets re-split whenever STT is re-run or its audio segmentation
// changes, which renumbers every chapter — and chunks carry chapter_idx, so the
// stale rows both under-cover the book and cite the wrong chapter. Skipping on
// "any chunks exist" made that permanent: King of Kings held chunks from a
// 16-chapter segmentation after re-splitting to 39 (34% of its words reachable),
// and Blueprint for Armageddon held 21-chapter chunks after re-splitting to 6
// (17%). Both looked fully chunked by row count.
func ChunkBook(store *db.Store, bookID int64) error {
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return err
	}

	count, _ := store.ChunkCount(bookID)
	if count > 0 {
		stale, err := chunksStale(store, bookID, chapters)
		if err != nil {
			return err
		}
		if !stale {
			return nil
		}
		// Rebuild from scratch: a partial top-up would leave the old rows'
		// chapter_idx pointing at chapters that have since been renumbered.
		log.Printf("chunker: book %d has %d stale chunk(s) — rebuilding", bookID, count)
		if err := store.DeleteChunksByBook(bookID); err != nil {
			return err
		}
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
