package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/library"
)

// Metadata editor (#70) — edit a work's title/author/series/description and its
// cover art (upload your own OR pick from an OpenLibrary search grid).

func (s *Server) coverPath(workID int64) string {
	return filepath.Join(s.LibraryDir, "covers", fmt.Sprintf("work-%d.jpg", workID))
}

// handleUpdateWorkMetadata is a FULL save of the editable metadata fields (a
// cleared field clears the value). Title is required.
func (s *Server) handleUpdateWorkMetadata(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Title       string  `json:"title"`
		Author      string  `json:"author"`
		Series      string  `json:"series"`
		SeriesIndex float64 `json:"series_index"`
		Description string  `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}
	if err := s.store.UpdateWorkMeta(workID, req.Title, strings.TrimSpace(req.Author),
		strings.TrimSpace(req.Series), req.SeriesIndex, strings.TrimSpace(req.Description)); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.stampWork(workID) // metadata is exportable → bump content_version for sync
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSearchCovers proxies an OpenLibrary cover search for the picker grid.
func (s *Server) handleSearchCovers(w http.ResponseWriter, r *http.Request) {
	title := r.URL.Query().Get("title")
	author := r.URL.Query().Get("author")
	if strings.TrimSpace(title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title required"})
		return
	}
	candidates, err := library.SearchOpenLibraryCovers(title, author, 12)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cover search failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": candidates})
}

// handleUploadCover accepts an uploaded image ("file") and sets it as the
// work's cover (validated + written atomically).
func (s *Server) handleUploadCover(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 15<<20)
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing image file"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 15<<20))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read upload"})
		return
	}
	if err := library.SaveCoverBytes(data, s.coverPath(workID)); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	s.coverUpdated(workID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePickCover sets the work's cover from a chosen OpenLibrary candidate URL.
func (s *Server) handlePickCover(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url required"})
		return
	}
	if err := library.FetchCoverToPath(req.URL, s.coverPath(workID)); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	s.coverUpdated(workID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// coverUpdated notifies clients + logs after a cover changes.
func (s *Server) coverUpdated(workID int64) {
	applog.Info("server", fmt.Sprintf("cover updated for work %d", workID))
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
}
