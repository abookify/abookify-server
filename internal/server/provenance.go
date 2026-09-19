package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/pj/abookify/internal/abook"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

// Provenance API (2026-09-18). A person DECLARES where a source came from and,
// separately, CLEARS it for public redistribution (cleared_by required). The
// public export (?public=1) and bin/publish-check read these rows; nothing
// here is inferred from titles or directories.
//
//   GET  /api/books/{id}/provenance          → the row (404 if undeclared)
//   PUT  /api/books/{id}/provenance          ← {kind, source_url, license, cleared, cleared_by, note}
//   GET  /api/works/{id}/provenance          → every book's row (or null) + the cover's sidecar + row,
//                                              and `publishable` = what a public export would do now
//   PUT  /api/works/{id}/cover/provenance    ← same body, scope=cover (an explicit cover clearance)

func (s *Server) handleGetBookProvenance(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows, err := s.store.GetSourceProvenance("book", []int64{id})
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	p, ok := rows[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no provenance declared for this book", "book_id": id})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) putProvenance(w http.ResponseWriter, r *http.Request, scope string, refID int64) {
	var body db.SourceProvenance
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	body.Scope, body.RefID = scope, refID
	if err := s.store.SetSourceProvenance(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rows, _ := s.store.GetSourceProvenance(scope, []int64{refID})
	writeJSON(w, http.StatusOK, rows[refID])
}

func (s *Server) handlePutBookProvenance(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if b, err := s.store.GetBook(id); err != nil || b == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such book"})
		return
	}
	s.putProvenance(w, r, "book", id)
}

func (s *Server) handlePutCoverProvenance(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if wk, err := s.store.GetWork(id); err != nil || wk == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such work"})
		return
	}
	s.putProvenance(w, r, "cover", id)
}

func (s *Server) handleWorkProvenance(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	wk, err := s.store.GetWork(id)
	if err != nil || wk == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such work"})
		return
	}
	var ids []int64
	for _, b := range wk.AudioFiles {
		ids = append(ids, b.ID)
	}
	for _, b := range wk.TextFiles {
		ids = append(ids, b.ID)
	}
	rows, err := s.store.GetSourceProvenance("book", ids)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	type entry struct {
		Book       db.Book              `json:"book"`
		Provenance *db.SourceProvenance `json:"provenance"`
	}
	var books []entry
	for _, b := range append(append([]db.Book{}, wk.AudioFiles...), wk.TextFiles...) {
		var pp *db.SourceProvenance
		if p, ok := rows[b.ID]; ok {
			cp := p
			pp = &cp
		}
		books = append(books, entry{Book: b, Provenance: pp})
	}
	coverPath := s.coverPath(id)
	src, hasSidecar := library.ReadCoverSource(coverPath)
	coverRows, _ := s.store.GetSourceProvenance("cover", []int64{id})
	var coverRow *db.SourceProvenance
	if p, ok := coverRows[id]; ok {
		cp := p
		coverRow = &cp
	}
	// What would a public export do right now? Dry-run the same verifier.
	verdict := map[string]any{"ok": true}
	if _, perr := abook.DryRunPublic(s.store, wk, s.LibraryDir); perr != nil {
		var nc *abook.ErrNotCleared
		if errors.As(perr, &nc) {
			verdict = map[string]any{"ok": false, "missing": nc.Missing}
		} else {
			verdict = map[string]any{"ok": false, "error": perr.Error()}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"work_id": id, "books": books,
		"cover":       map[string]any{"has_file": fileExists(coverPath), "source": src, "has_source_record": hasSidecar, "provenance": coverRow},
		"publishable": verdict,
	})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
