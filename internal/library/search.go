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
	WordPosition int     `json:"word_position"`       // approximate word offset in chapter
	Snippet      string  `json:"snippet"`             // ~100 chars of context around the match
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

	// Per-(book,chapter) composed word-map cache. The map gives each chapter word
	// its audio second in the CORRECT coordinate system — BuildTranscriptWordSync
	// slices the whole-work sync blob by the chapter's own audio window (fixing
	// the bug where a chapter-local word position was indexed straight into the
	// global sync array, so only chapter 0 ever resolved), and BuildEbookWordSync
	// maps a word-aligned ebook through the alignment. Built once per chapter.
	wmCache := map[[2]int64][]SyncWord{}
	wordMap := func(bookID int64, ch int) []SyncWord {
		k := [2]int64{bookID, int64(ch)}
		if wm, ok := wmCache[k]; ok {
			return wm
		}
		wm, _ := BuildDisplayWordSync(store, workID, bookID, ch)
		wmCache[k] = wm
		return wm
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

				// Audio timestamp. Prefer the composed per-chapter word map (correct
				// coordinate system for transcript chapters and word-aligned ebooks).
				// Fall back to the raw per-chapter sync index for TTS works whose
				// sync is keyed directly to the ebook chapter and have no
				// alignment/transcript to compose a map from — there the naive index
				// is already correct, so the fallback avoids regressing them.
				if len(work.AudioFiles) > 0 {
					af := work.AudioFiles[0]
					if wm := wordMap(tf.ID, chMeta.Index); wordPos < len(wm) {
						hit.AudioSec = wm[wordPos].S
						hit.AudioBookID = af.ID
					} else {
						raw, _ := store.GetSyncData(workID, af.ID, chMeta.Index)
						if raw == "" {
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

// LibraryHit is one passage match in the full library — same fields as
// SearchHit plus the work identity so the client can route to the
// right work card / reader.
type LibraryHit struct {
	WorkID     int64  `json:"work_id"`
	WorkTitle  string `json:"work_title"`
	WorkAuthor string `json:"work_author,omitempty"`
	SearchHit
}

// SearchLibrary runs SearchWork against every work in the library and
// returns up to `limit` hits across them, ordered work-by-work (the
// same order the library list shows). Limit is shared across works —
// once we collect `limit` hits we stop, regardless of which work we're
// in. Tiny query strings (< 2 chars) return empty to avoid scanning
// every chapter for single-letter matches.
func SearchLibrary(store *db.Store, query string, limit int) ([]LibraryHit, error) {
	query = strings.TrimSpace(query)
	if len(query) < 2 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	works, err := store.ListWorks()
	if err != nil {
		return nil, err
	}
	out := make([]LibraryHit, 0, limit)
	for _, w := range works {
		remaining := limit - len(out)
		if remaining <= 0 {
			break
		}
		hits, err := SearchWork(store, w.ID, query, remaining)
		if err != nil {
			continue
		}
		for _, h := range hits {
			out = append(out, LibraryHit{
				WorkID:     w.ID,
				WorkTitle:  w.Title,
				WorkAuthor: w.Author,
				SearchHit:  h,
			})
		}
	}
	return out, nil
}
