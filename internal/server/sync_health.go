package server

import (
	"net/http"
	"sort"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

// Sync-health is the DETECTABLE CHECK for the "dead sync" defect — a work that
// has an ebook + an alignment (so the reader SHOULD light up) but whose chapters
// yield no follow of any kind. The root cause is fixed in BuildTextSync (prefer
// the word map; honest `none`), and this endpoint makes the state QUERYABLE so
// "no book is silently dead" is verified by data rather than by eye. It runs
// BuildTextSync over each ebook's largest content chapters and reports the mode
// mix per work, worst first. GET /api/sync-health.

const (
	syncHealthMinWords  = 150 // a "content" chapter (skip title pages / dividers)
	syncHealthMaxSample = 12  // largest content chapters checked per work (bounded cost)
)

type syncHealthWork struct {
	WorkID          int64  `json:"work_id"`
	Title           string `json:"title"`
	BookID          int64  `json:"book_id"`
	ContentChapters int    `json:"content_chapters"`
	Word            int    `json:"word"`
	Paragraph       int    `json:"paragraph"`
	None            int    `json:"none"`
	Dead            bool   `json:"dead"` // has content chapters but NONE produce sync
}

func (s *Server) handleSyncHealth(w http.ResponseWriter, r *http.Request) {
	works, err := s.store.ListWorks()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	out := make([]syncHealthWork, 0)
	deadCount := 0
	for i := range works {
		work := &works[i]
		// Only works that are expected to sync: an ebook + an alignment.
		var ebook *db.Book
		for j := range work.TextFiles {
			if work.TextFiles[j].Origin != "whisper_transcript" && work.TextFiles[j].Format != "transcript" {
				ebook = &work.TextFiles[j]
				break
			}
		}
		if ebook == nil {
			continue
		}
		if aligns, _ := s.store.ListAlignmentsForWork(work.ID); len(aligns) == 0 {
			continue
		}
		chapters, _ := s.store.ListChapters(ebook.ID)
		var content []db.Chapter
		for _, ch := range chapters {
			if ch.WordCount >= syncHealthMinWords {
				content = append(content, ch)
			}
		}
		// Largest chapters first (most likely narrated), bounded sample.
		sort.Slice(content, func(a, b int) bool { return content[a].WordCount > content[b].WordCount })
		if len(content) > syncHealthMaxSample {
			content = content[:syncHealthMaxSample]
		}
		h := syncHealthWork{WorkID: work.ID, Title: work.Title, BookID: ebook.ID, ContentChapters: len(content)}
		for _, ch := range content {
			ts, e := library.BuildTextSync(s.store, work.ID, ebook.ID, ch.Index)
			switch {
			case e != nil || ts == nil:
				h.None++
			case ts.Mode == "word":
				h.Word++
			case ts.Mode == "paragraph" && len(ts.Spans) > 0:
				h.Paragraph++
			default:
				h.None++
			}
		}
		if h.ContentChapters > 0 && h.Word == 0 && h.Paragraph == 0 {
			h.Dead = true
			deadCount++
		}
		out = append(out, h)
	}
	// Worst first: dead works, then those with the most un-synced content chapters.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Dead != out[b].Dead {
			return out[a].Dead
		}
		return out[a].None > out[b].None
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"checked_works": len(out),
		"dead_count":    deadCount,
		"works":         out,
	})
}
