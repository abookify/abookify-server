package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/library"
)

// GET /api/queue/status — current snapshot of incoming/processing/failed.
func (s *Server) handleQueueStatus(w http.ResponseWriter, r *http.Request) {
	if s.Ingest == nil {
		// Queue not started (e.g. failed init). Return empty status rather
		// than 500 so the UI degrades gracefully.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"incoming":[],"processing":[],"failed":[]}`))
		return
	}
	status := s.Ingest.Status()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// DELETE /api/queue/failed/{name} — remove a failed entry.
// {name} must not contain path separators (defensive against traversal).
func (s *Server) handleQueueRemoveFailed(w http.ResponseWriter, r *http.Request) {
	if s.LibraryDir == "" {
		http.Error(w, "library not configured", http.StatusInternalServerError)
		return
	}
	name := r.PathValue("name")
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	target := filepath.Join(s.LibraryDir, "failed", name)
	if err := os.RemoveAll(target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Events.Broadcast(Event{Type: "queue_updated"})
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/works/{id}/reprocess — rerun the full post-processing pipeline
// against a work's existing sidecar. Cheap (seconds) since this only redoes
// chapter detection, transcript split, paragraphs, RAG chunks — never the
// expensive Whisper transcription.
//
// Contract: clobbers user-edited chapter rows. Documented limitation in the
// post-processing design talk; future work could honor a "source: user"
// flag to preserve hand-edits.
func (s *Server) handleReprocessWork(w http.ResponseWriter, r *http.Request) {
	if s.LibraryDir == "" {
		http.Error(w, "library not configured", http.StatusInternalServerError)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid work id", http.StatusBadRequest)
		return
	}
	if err := library.ReimportWork(s.store, id, s.LibraryDir); err != nil {
		// Specific error mapping: missing-sidecar / no-audio is 4xx
		// (the work isn't in a state where reprocess is meaningful);
		// import failures are 5xx.
		msg := err.Error()
		if strings.Contains(msg, "no sidecar") || strings.Contains(msg, "not found") || strings.Contains(msg, "no audio") {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	// LLM fallback: ask the configured provider to label any
	// "Chapter N" rows the narrator didn't title. No-op when no LLM
	// is configured.
	if rag := s.RAG(); rag != nil && rag.Client() != nil {
		if err := library.LabelMissingChapterTitles(s.store, rag.Client(), id); err != nil {
			// Non-fatal — chapters keep their bare titles, reprocess
			// still succeeds.
			s.Events.Broadcast(Event{Type: "library_updated"})
		}
	}
	// Tell connected clients to refresh their library view so chapters
	// + sync get reloaded from the new DB rows.
	s.Events.Broadcast(Event{Type: "library_updated"})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"work_id": id,
		"status":  "reprocessed",
	})
}

// GET /api/works/{id}/transcription-gaps — list audio spans where
// Whisper produced no output. Empty list = analyzed cleanly; missing
// = pre-gap-detection sidecar import, reprocess the work to populate.
func (s *Server) handleTranscriptionGaps(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid work id", http.StatusBadRequest)
		return
	}
	work, err := s.store.GetWork(id)
	if err != nil || work == nil {
		http.Error(w, "work not found", http.StatusNotFound)
		return
	}
	type bookGaps struct {
		BookID   int64           `json:"book_id"`
		Filename string          `json:"filename"`
		Analyzed bool            `json:"analyzed"`
		Gaps     json.RawMessage `json:"gaps"`
	}
	var out []bookGaps
	for _, b := range work.AudioFiles {
		raw, err := s.store.GetTranscriptionGaps(b.ID)
		if err != nil {
			continue
		}
		entry := bookGaps{BookID: b.ID, Filename: b.Filename}
		if raw == "" {
			entry.Analyzed = false
			entry.Gaps = json.RawMessage("[]")
		} else {
			entry.Analyzed = true
			entry.Gaps = json.RawMessage(raw)
		}
		out = append(out, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// GET /api/transcription-gaps/summary — one-shot rollup for the library
// page: returns one entry per work that has at least one detected gap,
// with the total missing time and a list of source files. The UI uses
// this to decorate work cards with a warning badge without N+1
// requests against the per-work endpoint.
func (s *Server) handleTranscriptionGapsSummary(w http.ResponseWriter, r *http.Request) {
	works, err := s.store.ListWorks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type summaryEntry struct {
		WorkID         int64    `json:"work_id"`
		TotalMissingS  float64  `json:"total_missing_sec"`
		SegmentCount   int      `json:"segment_count"`
		SourceFiles    []string `json:"source_files,omitempty"`
	}
	type gapShape struct {
		StartSec    float64 `json:"start_sec"`
		EndSec      float64 `json:"end_sec"`
		DurationSec float64 `json:"duration_sec"`
		SourceFile  string  `json:"source_file"`
	}
	var out []summaryEntry
	for _, wk := range works {
		var entry summaryEntry
		seen := map[string]bool{}
		for _, b := range wk.AudioFiles {
			raw, err := s.store.GetTranscriptionGaps(b.ID)
			if err != nil || raw == "" || raw == "[]" {
				continue
			}
			var gaps []gapShape
			if err := json.Unmarshal([]byte(raw), &gaps); err != nil {
				continue
			}
			for _, g := range gaps {
				entry.TotalMissingS += g.DurationSec
				entry.SegmentCount++
				if g.SourceFile != "" && !seen[g.SourceFile] {
					entry.SourceFiles = append(entry.SourceFiles, g.SourceFile)
					seen[g.SourceFile] = true
				}
			}
		}
		if entry.SegmentCount > 0 {
			entry.WorkID = wk.ID
			out = append(out, entry)
		}
	}
	if out == nil {
		out = []summaryEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// POST /api/books/{id}/embed — backfill chunk embeddings for one book.
// Idempotent (skips chunks that already have embeddings). Returns counts.
func (s *Server) handleEmbedBook(w http.ResponseWriter, r *http.Request) {
	rag := s.RAG()
	if rag == nil {
		http.Error(w, "RAG not configured (no LLM provider set)", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid book id", http.StatusBadRequest)
		return
	}
	embedded, err := rag.EmbedBook(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"book_id":  id,
		"embedded": embedded,
	})
}
