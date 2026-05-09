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
	// Tell connected clients to refresh their library view so chapters
	// + sync get reloaded from the new DB rows.
	s.Events.Broadcast(Event{Type: "library_updated"})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"work_id": id,
		"status":  "reprocessed",
	})
}

// POST /api/books/{id}/embed — backfill chunk embeddings for one book.
// Idempotent (skips chunks that already have embeddings). Returns counts.
func (s *Server) handleEmbedBook(w http.ResponseWriter, r *http.Request) {
	if s.RAG == nil {
		http.Error(w, "RAG not configured (no LLM provider set)", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid book id", http.StatusBadRequest)
		return
	}
	embedded, err := s.RAG.EmbedBook(id)
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
