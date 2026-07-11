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
	"github.com/pj/abookify/internal/db"
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
		Year        int     `json:"year"`
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
	// Guard against nonsense years; 0 means "unknown/clear".
	if req.Year != 0 && (req.Year < 1 || req.Year > 2200) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "year out of range"})
		return
	}
	if err := s.store.UpdateWorkMeta(workID, req.Title, strings.TrimSpace(req.Author),
		strings.TrimSpace(req.Series), req.SeriesIndex, strings.TrimSpace(req.Description), req.Year); err != nil {
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
	// Free-text `q` (the editable cover box) takes precedence; otherwise fall
	// back to the strict title/author fields.
	freeText := strings.TrimSpace(r.URL.Query().Get("q"))
	title := r.URL.Query().Get("title")
	author := r.URL.Query().Get("author")
	if freeText == "" && strings.TrimSpace(title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title or q required"})
		return
	}
	var candidates []library.CoverCandidate
	var err error
	if freeText != "" {
		candidates, err = library.SearchOpenLibraryCoversFreeText(freeText, 12)
	} else {
		candidates, err = library.SearchOpenLibraryCovers(title, author, 12)
	}
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

// handleDeleteSource removes ONE edition (audio or text book) from a work —
// used to dedupe a redundant edition or drop an unwanted one. Refuses to remove
// the work's last source. If the removed text book was the display default, the
// override is cleared so the resolver re-picks.
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	bookID, err := strconv.ParseInt(r.PathValue("bookId"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid book id"})
		return
	}
	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}
	// The book must belong to this work, and it can't be the only source left.
	owned := false
	total := len(work.AudioFiles) + len(work.TextFiles)
	for _, b := range append(append([]db.Book{}, work.AudioFiles...), work.TextFiles...) {
		if b.ID == bookID {
			owned = true
		}
	}
	if !owned {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that source doesn't belong to this work"})
		return
	}
	if total <= 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "can't remove the work's only source — delete the work instead"})
		return
	}
	if err := s.store.DeleteBook(bookID); err != nil {
		writeServerError(w, r, err)
		return
	}
	if work.DisplayTextBookID == bookID {
		s.store.SetDisplayTextBook(workID, 0) // clear the now-dangling override
	}
	s.stampWork(workID)
	applog.Info("server", fmt.Sprintf("removed source %d from work %d", bookID, workID))
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// handleRelabelEdition sets the edition label on a set of the work's books (the
// metadata editor's per-edition "Rename" action). Every id must belong to the
// work. An empty label clears it back to the default edition.
func (s *Server) handleRelabelEdition(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		BookIDs []int64 `json:"book_ids"`
		Edition string  `json:"edition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.BookIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "book_ids required"})
		return
	}
	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}
	owned := map[int64]bool{}
	for _, b := range append(append([]db.Book{}, work.AudioFiles...), work.TextFiles...) {
		owned[b.ID] = true
	}
	for _, id := range req.BookIDs {
		if !owned[id] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a source doesn't belong to this work"})
			return
		}
	}
	if err := s.store.SetBookEdition(workID, req.BookIDs, strings.TrimSpace(req.Edition)); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.stampWork(workID)
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// coverUpdated notifies clients + logs after a cover changes.
func (s *Server) coverUpdated(workID int64) {
	applog.Info("server", fmt.Sprintf("cover updated for work %d", workID))
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
}
