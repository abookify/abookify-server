package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/abook"
	"github.com/pj/abookify/internal/db"
)

// newTestServer builds a Server backed by a fresh temp store. LibraryDir
// points at a temp dir so export/cover paths stay isolated. Handlers are
// invoked directly (bypassing the auth middleware) with httptest.
func newTestServer(t *testing.T) (*Server, *db.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "monolith.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	srv := New(store, "0")
	srv.LibraryDir = dir
	return srv, store, dir
}

// seedAligned creates an audio+text work with an alignment + version stamp and
// returns its id. Mirrors the abook round-trip fixture.
func seedAligned(t *testing.T, store *db.Store, dir string) int64 {
	t.Helper()
	workID, err := store.CreateWork("Test Book", "Ada Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	audioPath := filepath.Join(dir, "ch01.mp3")
	os.WriteFile(audioPath, []byte("fake audio"), 0644)
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: audioPath, Filename: "ch01.mp3",
		Format: "mp3", MediaType: "audio", Title: "Chapter 1", Origin: "narrator_recording",
	}); err != nil {
		t.Fatalf("upsert audio: %v", err)
	}
	textPath := filepath.Join(dir, "book.epub")
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: textPath, Filename: "book.epub",
		Format: "epub", MediaType: "text", Title: "Test Book", Origin: "publisher_epub",
	}); err != nil {
		t.Fatalf("upsert text: %v", err)
	}
	var audioID, textID int64
	books, _ := store.ListBooks()
	for _, b := range books {
		if b.Path == audioPath {
			audioID = b.ID
		}
		if b.Path == textPath {
			textID = b.ID
		}
	}
	store.InsertChapter(db.Chapter{BookID: textID, Index: 0, Title: "Chapter 1", Content: "Hello world.", WordCount: 2})
	if err := store.SaveAlignment(db.Alignment{
		WorkID: workID, FromBookID: audioID, ToBookID: textID,
		Unit: "word", Confidence: 0.9, Method: "anchor", Pairs: "[]",
	}); err != nil {
		t.Fatalf("save alignment: %v", err)
	}
	if err := store.StampVersions(workID, abook.BookDBSchemaVersion); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	return workID
}

func TestHandleWorkVersion(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID := seedAligned(t, store, dir)

	// 200 with both stamps.
	req := httptest.NewRequest("GET", "/api/works/x/version", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleWorkVersion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		SchemaVersion  int    `json:"schema_version"`
		ContentVersion string `json:"content_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SchemaVersion != abook.BookDBSchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, abook.BookDBSchemaVersion)
	}
	if got.ContentVersion == "" {
		t.Errorf("content_version empty")
	}

	// 404 for a missing work.
	req = httptest.NewRequest("GET", "/api/works/999999/version", nil)
	req.SetPathValue("id", "999999")
	rec = httptest.NewRecorder()
	srv.handleWorkVersion(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing work status = %d, want 404", rec.Code)
	}

	// 400 for a non-numeric id.
	req = httptest.NewRequest("GET", "/api/works/abc/version", nil)
	req.SetPathValue("id", "abc")
	rec = httptest.NewRecorder()
	srv.handleWorkVersion(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad id status = %d, want 400", rec.Code)
	}
}

func TestHandleCatalog(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID := seedAligned(t, store, dir)

	req := httptest.NewRequest("GET", "/api/catalog", nil)
	rec := httptest.NewRecorder()
	srv.handleCatalog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var cat []struct {
		ID             int64    `json:"id"`
		SourceKind     string   `json:"source_kind"`
		CoveragePct    *float64 `json:"coverage_pct"`
		AlignMethod    *string  `json:"align_method"`
		HasAudio       bool     `json:"has_audio"`
		HasText        bool     `json:"has_text"`
		ContentVersion string   `json:"content_version"`
		SchemaVersion  int      `json:"schema_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cat) != 1 {
		t.Fatalf("got %d entries, want 1", len(cat))
	}
	e := cat[0]
	if e.ID != workID {
		t.Errorf("id = %d, want %d", e.ID, workID)
	}
	if e.SourceKind != "aligned" {
		t.Errorf("source_kind = %q, want aligned", e.SourceKind)
	}
	if !e.HasAudio || !e.HasText {
		t.Errorf("has_audio=%v has_text=%v, want both true", e.HasAudio, e.HasText)
	}
	if e.CoveragePct == nil || *e.CoveragePct < 89 || *e.CoveragePct > 91 {
		t.Errorf("coverage_pct = %v, want ~90", e.CoveragePct)
	}
	if e.AlignMethod == nil || *e.AlignMethod != "anchor" {
		t.Errorf("align_method = %v, want anchor", e.AlignMethod)
	}
	if e.SchemaVersion != abook.BookDBSchemaVersion || e.ContentVersion == "" {
		t.Errorf("versions = %d/%q", e.SchemaVersion, e.ContentVersion)
	}
}

func TestHandleExportsListAndDownload(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID := seedAligned(t, store, dir)

	// Write a v2 export (no audio) into the exports dir, as export-all would.
	exportDir := filepath.Join(dir, "exports")
	if err := os.MkdirAll(exportDir, 0755); err != nil {
		t.Fatal(err)
	}
	work, _ := store.GetWork(workID)
	out := filepath.Join(exportDir, "work-"+itoa(workID)+".abook")
	if err := abook.ExportV2(store, work, out, dir, abook.ExportOptions{IncludeAudio: false}); err != nil {
		t.Fatalf("export: %v", err)
	}

	// List.
	rec := httptest.NewRecorder()
	srv.handleListExports(rec, httptest.NewRequest("GET", "/api/exports", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list []struct {
		File       string `json:"file"`
		WorkID     int64  `json:"work_id"`
		SourceKind string `json:"source_kind"`
		SizeBytes  int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].WorkID != workID || list[0].SizeBytes == 0 {
		t.Fatalf("list = %+v", list)
	}

	// Download the listed file.
	req := httptest.NewRequest("GET", "/api/exports/"+list[0].File, nil)
	req.SetPathValue("file", list[0].File)
	rec = httptest.NewRecorder()
	srv.handleGetExport(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("download status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-abook+zip" {
		t.Errorf("content-type = %q", ct)
	}
	if rec.Body.Len() == 0 {
		t.Errorf("empty download body")
	}

	// Path-traversal attempt is reduced to a basename and rejected (not .abook).
	req = httptest.NewRequest("GET", "/api/exports/x", nil)
	req.SetPathValue("file", "../../monolith.db")
	rec = httptest.NewRecorder()
	srv.handleGetExport(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("traversal status = %d, want 400", rec.Code)
	}
}

func TestHandleListExportsEmpty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleListExports(rec, httptest.NewRequest("GET", "/api/exports", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
}

// itoa avoids importing strconv solely for path-value formatting in tests.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
