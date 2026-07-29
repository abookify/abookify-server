package server

// Library-roots management API (#220). Web + mobile render the per-root state
// (path, label, book count, reachable/offline) so a dead mount is VISIBLE rather
// than the library silently shrinking.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/library"
)

// handleListRoots: GET /api/library/roots — roots with live reachability + counts.
func (s *Server) handleListRoots(w http.ResponseWriter, r *http.Request) {
	roots, err := s.store.ListRoots()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(roots))
	for _, rt := range roots {
		total, stale, _ := s.store.CountBooksUnderRoot(rt.ID)
		out = append(out, map[string]any{
			"id":          rt.ID,
			"path":        rt.Path,
			"label":       rt.Label,
			"is_default":  rt.IsDefault,
			"position":    rt.Position,
			"reachable":   library.RootReachable(rt.Path),
			"book_count":  total,
			"stale_count": stale,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": out})
}

// pathsOverlap reports whether two absolute paths are equal or nested (one is a
// parent of the other) — nested roots are rejected so a file isn't double-scanned.
func pathsOverlap(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+string(filepath.Separator)) ||
		strings.HasPrefix(b, a+string(filepath.Separator))
}

func dirHasEntries(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

// handleAddRoot: POST /api/library/roots {path, label}. Validates the path is an
// existing directory that doesn't overlap an existing root, adds it, marks it
// reachable when it already has content, and kicks off a scan.
func (s *Server) handleAddRoot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string `json:"path"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	path := filepath.Clean(strings.TrimSpace(req.Path))
	if path == "" || !filepath.IsAbs(path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "an absolute path is required"})
		return
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is not an existing directory (is the drive mounted?)"})
		return
	}
	roots, _ := s.store.ListRoots()
	for _, rt := range roots {
		if pathsOverlap(path, rt.Path) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "overlaps an existing library location: " + rt.Path})
			return
		}
	}
	id, err := s.store.AddRoot(path, strings.TrimSpace(req.Label))
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if dirHasEntries(path) {
		library.MarkRootReachable(path)
	}
	// Scan the new root so its books show up (background — don't block the add).
	go func() {
		if _, err := Rescan(s.store, path); err != nil {
			applog.Warnf("system", "scan of new library root %q failed: %v", path, err)
		}
		s.store.AssignBooksToRoot(id, path)
		if s.Events != nil {
			s.Events.Broadcast(Event{Type: "library_updated"})
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id})
}

// handleDeleteRoot: DELETE /api/library/roots/{id}. Books are unassigned, not
// deleted (no metadata loss).
func (s *Server) handleDeleteRoot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := s.store.RemoveRoot(id); err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// handlePatchRoot: PATCH /api/library/roots/{id} {is_default?, label?}.
func (s *Server) handlePatchRoot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		IsDefault *bool   `json:"is_default,omitempty"`
		Label     *string `json:"label,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if req.IsDefault != nil && *req.IsDefault {
		if err := s.store.SetDefaultRoot(id); err != nil {
			writeServerError(w, r, err)
			return
		}
	}
	if req.Label != nil {
		if err := s.store.SetRootLabel(id, strings.TrimSpace(*req.Label)); err != nil {
			writeServerError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReorderRoots: POST /api/library/roots/reorder {ids: []}.
func (s *Server) handleReorderRoots(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids required"})
		return
	}
	if err := s.store.ReorderRoots(req.IDs); err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
