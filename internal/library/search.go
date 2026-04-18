// Within-book text search. Searches chapter content across all text books
// in a work, returns snippets with chapter/word position and audio timestamps
// when sync_data is available.
package library

import (
	"strings"

	"github.com/pj/abookify/internal/db"
)

// SearchHit is one search result within a work.
type SearchHit struct {
	BookID       int64   `json:"book_id"`
	BookTitle    string  `json:"book_title"`
	ChapterIdx   int     `json:"chapter_idx"`
	ChapterTitle string  `json:"chapter_title"`
	WordPosition int     `json:"word_position"` // approximate word offset in chapter
	Snippet      string  `json:"snippet"`       // ~100 chars of context around the match
	AudioSec     float64 `json:"audio_sec,omitempty"` // audio timestamp if sync_data available
	AudioBookID  int64   `json:"audio_book_id,omitempty"`
}

// SearchWork searches all text books in a work for a query string. Returns
// up to `limit` hits with snippets and optional audio timestamps.
func SearchWork(store *db.Store, workID int64, query string, limit int) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 20
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	queryLower := strings.ToLower(query)

	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return nil, err
	}

	var hits []SearchHit
	for _, tf := range work.TextFiles {
		if tf.Visibility == "internal" {
			continue
		}
		chapters, err := store.ListChapters(tf.ID)
		if err != nil {
			continue
		}
		for _, chMeta := range chapters {
			ch, err := store.GetChapterContent(tf.ID, chMeta.Index)
			if err != nil || ch == nil {
				continue
			}
			contentLower := strings.ToLower(ch.Content)
			searchFrom := 0
			for searchFrom < len(contentLower) {
				idx := strings.Index(contentLower[searchFrom:], queryLower)
				if idx < 0 {
					break
				}
				absIdx := searchFrom + idx
				// Build snippet: ~50 chars before + match + ~50 chars after.
				snippetStart := absIdx - 50
				if snippetStart < 0 {
					snippetStart = 0
				}
				snippetEnd := absIdx + len(query) + 50
				if snippetEnd > len(ch.Content) {
					snippetEnd = len(ch.Content)
				}
				snippet := ch.Content[snippetStart:snippetEnd]
				if snippetStart > 0 {
					snippet = "..." + snippet
				}
				if snippetEnd < len(ch.Content) {
					snippet = snippet + "..."
				}

				// Approximate word position: count words before the match.
				wordPos := len(strings.Fields(ch.Content[:absIdx]))

				hit := SearchHit{
					BookID:       tf.ID,
					BookTitle:    tf.Title,
					ChapterIdx:   chMeta.Index,
					ChapterTitle: chMeta.Title,
					WordPosition: wordPos,
					Snippet:      snippet,
				}

				// Try to get audio timestamp via sync_data.
				if len(work.AudioFiles) > 0 {
					af := work.AudioFiles[0]
					raw, _ := store.GetSyncData(workID, af.ID, chMeta.Index)
					if raw == "" {
						// Single-file books store all sync_data at chapter_idx=0
						raw, _ = store.GetSyncData(workID, af.ID, 0)
					}
					if raw != "" {
						var ts []db.SyncTimestamp
						if err := jsonUnmarshal(raw, &ts); err == nil && wordPos < len(ts) {
							hit.AudioSec = ts[wordPos].Start
							hit.AudioBookID = af.ID
						}
					}
				}

				hits = append(hits, hit)
				if len(hits) >= limit {
					return hits, nil
				}
				searchFrom = absIdx + len(query)
			}
		}
	}
	return hits, nil
}
