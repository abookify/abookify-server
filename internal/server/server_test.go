package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/abook"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
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

func TestHandleWorkDiff(t *testing.T) {
	srv, store, dir := newTestServer(t)
	_ = dir

	workID, _ := store.CreateWork("Diff Book", "Author")
	// EPUB side.
	store.UpsertBook(db.Book{WorkID: workID, Path: "/tmp/diff-ebook.epub", Filename: "e.epub",
		Format: "epub", MediaType: "text", Title: "Diff Book", Origin: "publisher_epub"})
	ebookID := bookIDByPath(t, store, "/tmp/diff-ebook.epub")
	store.InsertChapter(db.Chapter{BookID: ebookID, Index: 0, Title: "Chapter 1",
		Content: "alpha bravo charlie delta echo", WordCount: 5})
	// Transcript side.
	store.UpsertBook(db.Book{WorkID: workID, Path: "/tmp/diff-trans.txt", Filename: "t.txt",
		Format: "transcript", MediaType: "text", Title: "Diff Book", Origin: "whisper_transcript"})
	transID := bookIDByPath(t, store, "/tmp/diff-trans.txt")
	store.InsertChapter(db.Chapter{BookID: transID, Index: 0, Title: "Chapter 1",
		Content: "alpha bravo foxtrot", WordCount: 3})

	payload := library.AnchorAlignmentPayload{
		Method: "anchor", Unit: "word", EbookWords: 5, TransWords: 3, Coverage: 0.4,
		Segments: []library.Segment{
			{EbookStart: 0, EbookEnd: 2, TransStart: 0, TransEnd: 2, Kind: library.SegAligned},
			{EbookStart: 2, EbookEnd: 5, TransStart: 2, TransEnd: 2, Kind: library.SegEbookOnly},
			{EbookStart: 5, EbookEnd: 5, TransStart: 2, TransEnd: 3, Kind: library.SegTransOnly},
		},
	}
	pj, _ := json.Marshal(payload)
	if err := store.SaveAlignment(db.Alignment{WorkID: workID, FromBookID: ebookID, ToBookID: transID,
		Unit: "word", Confidence: 0.4, Method: "anchor", Pairs: string(pj)}); err != nil {
		t.Fatalf("save alignment: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/works/x/diff", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleWorkDiff(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var d library.WorkDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.SourceA.Origin != "publisher_epub" || d.SourceB.Origin != "whisper_transcript" {
		t.Errorf("sources = %q / %q", d.SourceA.Origin, d.SourceB.Origin)
	}
	if len(d.Spans) != 3 {
		t.Fatalf("spans = %d, want 3", len(d.Spans))
	}
	// Aligned: text omitted, counts kept.
	if d.Spans[0].Kind != "aligned" || d.Spans[0].AText != "" || d.Spans[0].AWords != 2 {
		t.Errorf("aligned span = %+v", d.Spans[0])
	}
	// ebook-only: original-case text recovered from offsets.
	if d.Spans[1].Kind != "ebook-only" || d.Spans[1].AText != "charlie delta echo" || d.Spans[1].BWords != 0 {
		t.Errorf("ebook-only span = %+v", d.Spans[1])
	}
	// trans-only: transcript text recovered.
	if d.Spans[2].Kind != "trans-only" || d.Spans[2].BText != "foxtrot" {
		t.Errorf("trans-only span = %+v", d.Spans[2])
	}
}

func TestHandleWorkDiff404(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID := seedAligned(t, store, dir) // alignment Pairs="[]" → no segments
	req := httptest.NewRequest("GET", "/api/works/x/diff", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleWorkDiff(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGetCastGracefulDefault(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID := seedAligned(t, store, dir)

	req := httptest.NewRequest("GET", "/api/works/x/cast", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleGetCast(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Experimental bool          `json:"experimental"`
		Enabled      bool          `json:"enabled"`
		Characters   []db.Character `json:"characters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Experimental {
		t.Errorf("experimental should always be true")
	}
	if got.Enabled {
		t.Errorf("enabled should be false by default (no flag, no service)")
	}
	if len(got.Characters) != 0 {
		t.Errorf("characters should be empty by default, got %d", len(got.Characters))
	}
}

func TestHandleExtractCastGatedByFlag(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID := seedAligned(t, store, dir)

	// Flag off → 403 regardless of service URL.
	req := httptest.NewRequest("POST", "/api/works/x/extract-cast", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleExtractCast(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("flag-off status = %d, want 403", rec.Code)
	}

	// Flag on but no service URL → 503.
	store.SetSetting("booknlp_enabled", "true")
	req = httptest.NewRequest("POST", "/api/works/x/extract-cast", nil)
	req.SetPathValue("id", itoa(workID))
	rec = httptest.NewRecorder()
	srv.handleExtractCast(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no-service status = %d, want 503", rec.Code)
	}
}

// zipEntries returns the set of entry names in a zip held in body.
func zipEntries(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("open zip from response: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	return names
}

func TestHandleExportAbookAudioToggle(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID := seedAligned(t, store, dir)
	audioEntry := "audio/book-" + itoa(bookIDByPath(t, store, filepath.Join(dir, "ch01.mp3"))) + ".mp3"

	// Default: audio bundled.
	req := httptest.NewRequest("GET", "/api/works/x/export.abook", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleExportAbook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("default export status = %d", rec.Code)
	}
	full := zipEntries(t, rec.Body.Bytes())
	if !full["book.db"] || !full["manifest.json"] {
		t.Errorf("default export missing core entries: %v", full)
	}
	if !full[audioEntry] {
		t.Errorf("default export should bundle audio (%s); have %v", audioEntry, full)
	}

	// audio=0: lightweight, no audio dir.
	req = httptest.NewRequest("GET", "/api/works/x/export.abook?audio=0", nil)
	req.SetPathValue("id", itoa(workID))
	rec = httptest.NewRecorder()
	srv.handleExportAbook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-audio export status = %d", rec.Code)
	}
	lite := zipEntries(t, rec.Body.Bytes())
	if !lite["book.db"] || !lite["manifest.json"] {
		t.Errorf("no-audio export missing core entries: %v", lite)
	}
	if lite[audioEntry] {
		t.Errorf("no-audio export should not bundle audio; have %v", lite)
	}
}

// bookIDByPath resolves a book's server id by its unique path.
func bookIDByPath(t *testing.T, store *db.Store, path string) int64 {
	t.Helper()
	books, _ := store.ListBooks()
	for _, b := range books {
		if b.Path == path {
			return b.ID
		}
	}
	t.Fatalf("book not found for path %q", path)
	return 0
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
