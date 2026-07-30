package server

import (
	"net/http"
	"sort"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

// Sync-health is the DETECTABLE CHECK for the "dead sync" defect — a displayable
// source (an ebook + alignment, or the transcript) that SHOULD light up but whose
// chapters yield no follow of any kind. The root cause is fixed in BuildTextSync
// (prefer the word map; honest `none`), and this endpoint makes the state
// QUERYABLE so "no book is silently dead" is verified by data rather than by eye.
//
// Word-ness is judged from the REAL word map, never a bare mode string, so a
// chapter that would report mode=word but delivers an empty map can never count
// as a pass here (the lie this endpoint exists to catch). Ebooks go through
// BuildTextSync, whose word decision is already gated on a non-empty composed
// map; transcripts are word-timed by construction, so their whole sync blob is
// parsed ONCE per work and each chapter is judged by whether any word overlaps
// its audio window (cheap — no per-chapter re-parse). GET /api/sync-health.

const (
	syncHealthMinWords  = 150 // a "content" chapter (skip title pages / dividers)
	syncHealthMaxSample = 12  // largest content chapters checked per source (bounded cost)
)

type syncHealthWork struct {
	WorkID          int64  `json:"work_id"`
	Title           string `json:"title"`
	BookID          int64  `json:"book_id"`
	Source          string `json:"source"` // "ebook" | "transcript" — which displayed source was checked
	ContentChapters int    `json:"content_chapters"`
	Word            int    `json:"word"`
	Paragraph       int    `json:"paragraph"`
	None            int    `json:"none"`
	Dead            bool   `json:"dead"` // has content chapters but NONE produce usable sync
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
		aligns, _ := s.store.ListAlignmentsForWork(work.ID)
		transIDs := map[int64]bool{}
		for j := range work.TextFiles {
			b := &work.TextFiles[j]
			if b.Origin == "whisper_transcript" || b.Format == "transcript" {
				transIDs[b.ID] = true
			}
		}
		// Does THIS ebook have a pairing alignment with a transcript? Work-level
		// "has an alignment" isn't enough — a work can hold several ebook editions
		// where only one is aligned (Frankenstein: two EPUBs, one aligned, one not).
		// An un-aligned edition legitimately has no sync, so it's "not expected to
		// sync" and must not be flagged dead (mirrors BuildTextSync's best==nil→none).
		ebookPaired := func(bookID int64) bool {
			for k := range aligns {
				a := &aligns[k]
				if (a.FromBookID == bookID && transIDs[a.ToBookID]) ||
					(a.ToBookID == bookID && transIDs[a.FromBookID]) {
					return true
				}
			}
			return false
		}
		// Check every displayable text source expected to sync: an ebook (needs its
		// OWN alignment) AND the transcript (word-timed by construction). The
		// transcript was NOT checked before — a transcript reporting word with an
		// empty map would slip through as "fine".
		for j := range work.TextFiles {
			b := &work.TextFiles[j]
			isTranscript := b.Origin == "whisper_transcript" || b.Format == "transcript"
			if !isTranscript && !ebookPaired(b.ID) {
				continue // an un-aligned ebook edition isn't expected to sync
			}
			source := "ebook"
			if isTranscript {
				source = "transcript"
			}
			chapters, _ := s.store.ListChapters(b.ID)
			var content []db.Chapter
			for _, ch := range chapters {
				if ch.WordCount >= syncHealthMinWords {
					content = append(content, ch)
				}
			}
			sort.Slice(content, func(a, c int) bool { return content[a].WordCount > content[c].WordCount })
			if len(content) > syncHealthMaxSample {
				content = content[:syncHealthMaxSample]
			}
			h := syncHealthWork{WorkID: work.ID, Title: work.Title, BookID: b.ID, Source: source, ContentChapters: len(content)}

			if isTranscript {
				// Parse the word blob ONCE; a chapter is word-synced iff a word overlaps
				// its [start,end) window — the same truth /word-sync delivers, so a
				// hollow word claim is impossible here by construction.
				allWords, _ := library.LoadWorkSyncWords(s.store, work.ID)
				byIdx := map[int][2]float64{}
				for _, ch := range chapters {
					byIdx[ch.Index] = [2]float64{ch.StartSec, ch.EndSec}
				}
				for _, ch := range content {
					win := byIdx[ch.Index]
					if len(library.SliceTranscriptChapter(allWords, win[0], win[1])) > 0 {
						h.Word++
					} else {
						h.None++
					}
				}
			} else {
				for _, ch := range content {
					ts, e := library.BuildTextSync(s.store, work.ID, b.ID, ch.Index)
					switch {
					case e != nil || ts == nil:
						h.None++
					case ts.Mode == "word": // BuildTextSync gates word on a non-empty ebook map
						h.Word++
					case ts.Mode == "paragraph" && len(ts.Spans) > 0:
						h.Paragraph++
					default:
						h.None++
					}
				}
			}

			if h.ContentChapters > 0 && h.Word == 0 && h.Paragraph == 0 {
				h.Dead = true
				deadCount++
			}
			out = append(out, h)
		}
	}
	// Worst first: dead sources, then those with the most un-synced content chapters.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Dead != out[b].Dead {
			return out[a].Dead
		}
		return out[a].None > out[b].None
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"checked_sources": len(out),
		"dead_count":      deadCount,
		"works":           out,
	})
}
