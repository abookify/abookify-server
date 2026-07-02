package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/abook"
)

// Device→server .abook upload dedupes by title+author: a re-upload of a work we
// already have returns a 409 conflict for the client to resolve, and
// ?on_conflict=skip|replace|new act as expected.
func TestHandleImportAbookDedup(t *testing.T) {
	srv, store, dir := newTestServer(t)

	// Build a .abook from a seeded work, then remove the seed so the library
	// starts empty (a clean first import).
	workID := seedAligned(t, store, dir) // "Test Book" / "Ada Author"
	work, _ := store.GetWork(workID)
	abookPath := filepath.Join(dir, "x.abook")
	if err := abook.ExportV2(store, work, abookPath, dir, abook.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := store.DeleteWork(workID); err != nil {
		t.Fatalf("delete seed: %v", err)
	}

	post := func(query string) (int, map[string]any) {
		t.Helper()
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		fw, _ := mw.CreateFormFile("file", "x.abook")
		data, _ := os.ReadFile(abookPath)
		fw.Write(data)
		mw.Close()
		req := httptest.NewRequest("POST", "/api/import"+query, body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		srv.handleImportAbook(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// 1. First import → created.
	if code, out := post(""); code != http.StatusOK || out["status"] != "imported" {
		t.Fatalf("first import = %d %v, want 200 imported", code, out)
	}
	// 2. Second import (default) → 409 conflict with generation info.
	code, out := post("")
	if code != http.StatusConflict || out["status"] != "conflict" {
		t.Fatalf("dup import = %d %v, want 409 conflict", code, out)
	}
	if _, ok := out["incoming_newer"]; !ok || out["existing_work_id"] == nil {
		t.Errorf("conflict payload missing fields: %v", out)
	}
	// 3. skip → no-op.
	if code, out := post("?on_conflict=skip"); code != http.StatusOK || out["status"] != "skipped" {
		t.Errorf("skip = %d %v, want skipped", code, out)
	}
	// 4. replace → swaps in place; exactly one work remains.
	if code, out := post("?on_conflict=replace"); code != http.StatusOK || out["status"] != "replaced" {
		t.Errorf("replace = %d %v, want replaced", code, out)
	}
	works, _ := store.ListWorks()
	if len(works) != 1 {
		t.Errorf("after replace: %d works, want 1", len(works))
	}
	// 5. new → keep both (2 works with the same title now).
	if code, out := post("?on_conflict=new"); code != http.StatusOK || out["status"] != "imported" {
		t.Errorf("new = %d %v, want imported", code, out)
	}
	if works, _ := store.ListWorks(); len(works) != 2 {
		t.Errorf("after new: %d works, want 2", len(works))
	}
}

// Import preserves the .abook's content_version (generation stamp) instead of
// stamping the import time — required for dedupe-by-generation.
func TestImportPreservesContentVersion(t *testing.T) {
	_, store, dir := newTestServer(t)
	workID := seedAligned(t, store, dir)
	store.SetContentVersion(workID, "2020-01-02T03:04:05Z") // a known old stamp
	work, _ := store.GetWork(workID)
	abookPath := filepath.Join(dir, "y.abook")
	if err := abook.ExportV2(store, work, abookPath, dir, abook.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}
	store.DeleteWork(workID)

	if err := abook.Import(store, abookPath, dir); err != nil {
		t.Fatalf("import: %v", err)
	}
	_, cv, found, _ := store.FindWorkByTitleAuthor("Test Book", "Ada Author")
	if !found || cv != "2020-01-02T03:04:05Z" {
		t.Errorf("content_version = %q (found=%v), want the manifest's 2020 stamp", cv, found)
	}
}
