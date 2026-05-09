package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
