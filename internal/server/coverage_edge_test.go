package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

// #199/#200: a work with no alignment still answers 200 with an empty (never
// null) pairs list — the listing/work readout must not error on un-aligned works.
func TestHandleWorkCoverageEmpty(t *testing.T) {
	srv, store, _ := newTestServer(t)
	workID, _ := store.CreateWork("Unaligned", "Author")
	store.UpsertBook(db.Book{WorkID: workID, Path: "/tmp/u.epub", Filename: "u.epub",
		Format: "epub", MediaType: "text", Title: "Unaligned", Origin: "publisher_epub"})

	req := httptest.NewRequest("GET", "/api/works/x/coverage", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleWorkCoverage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var cov library.WorkCoverage
	if err := json.Unmarshal(rec.Body.Bytes(), &cov); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cov.Pairs == nil {
		t.Error("pairs is null, want [] (empty list)")
	}
	if len(cov.Pairs) != 0 {
		t.Errorf("pairs = %d, want 0 (no alignment)", len(cov.Pairs))
	}
}

// #199/#200: a non-numeric work id is a 400.
func TestHandleWorkCoverageInvalidID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/works/x/coverage", nil)
	req.SetPathValue("id", "nope")
	rec := httptest.NewRecorder()
	srv.handleWorkCoverage(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// #202: the settings-schema endpoint serves the versioned source-of-truth that
// web + mobile render from — non-empty groups, stamped version.
func TestHandleSettingsSchemaEndpoint(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/settings/schema", nil)
	rec := httptest.NewRecorder()
	srv.handleSettingsSchema(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var doc SettingsSchemaDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Version != SettingsSchemaVersion {
		t.Errorf("version = %d, want %d", doc.Version, SettingsSchemaVersion)
	}
	if len(doc.Groups) == 0 {
		t.Error("groups is empty, want the tts/stt/llm/… field set")
	}
}
