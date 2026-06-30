package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// When BookNLP is configured but NOT running (the default — it's opt-in), an
// extract-cast must fail SOFT: a 503 with an actionable "start it" message,
// never a bare 500. This is the exact bug PJ hit clicking "Extract cast".
func TestHandleExtractCastServiceDown(t *testing.T) {
	srv, store, dir := newTestServer(t)

	// A reliably-closed port → connection refused (no real booknlp).
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	srv.BookNLPURL = "http://" + addr

	store.SetSetting("booknlp_enabled", "true")
	workID, _ := store.CreateWork("Frankenstein", "Shelley")
	epub := dir + "/f.epub"
	store.UpsertBook(db.Book{WorkID: workID, Path: epub, Filename: "f.epub",
		Format: "epub", MediaType: "text", Title: "Frankenstein", Origin: "publisher_epub"})
	bookID := bookIDByPath(t, store, epub)
	store.InsertChapter(db.Chapter{BookID: bookID, Index: 0, Title: "Ch1",
		Content: strings.Repeat("word ", 50), WordCount: 50})

	req := httptest.NewRequest("POST", "/api/works/x/extract-cast", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleExtractCast(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (graceful, not 500)", rec.Code)
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.Contains(out["error"], "BookNLP") || !strings.Contains(out["error"], "--profile booknlp") {
		t.Errorf("error message not actionable: %q", out["error"])
	}
	if strings.Contains(out["error"], "internal server error") {
		t.Errorf("leaked a bare 500 message: %q", out["error"])
	}
}

// #133: the cast endpoint always reports experimental:true, gates `enabled`
// on BOTH the booknlp_enabled flag and a configured service URL, and returns
// an empty (never null) characters list when there's no cast.
func TestHandleGetCast(t *testing.T) {
	srv, store, _ := newTestServer(t)

	workID, _ := store.CreateWork("Cast Book", "Author")
	store.UpsertBook(db.Book{WorkID: workID, Path: "/tmp/cast.epub", Filename: "c.epub",
		Format: "epub", MediaType: "text", Title: "Cast Book", Origin: "publisher_epub"})
	bookID := bookIDByPath(t, store, "/tmp/cast.epub")

	getCast := func() (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/works/x/cast", nil)
		req.SetPathValue("id", itoa(workID))
		rec := httptest.NewRecorder()
		srv.handleGetCast(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// No cast, feature off → experimental:true, enabled:false, characters:[].
	code, out := getCast()
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out["experimental"] != true {
		t.Errorf("experimental = %v, want true (mandatory on every cast surface)", out["experimental"])
	}
	if out["enabled"] != false {
		t.Errorf("enabled = %v, want false (flag off, no service)", out["enabled"])
	}
	if chars, ok := out["characters"].([]any); !ok || len(chars) != 0 {
		t.Errorf("characters = %v, want [] (empty, not null)", out["characters"])
	}

	// Flag on but no service URL → still NOT enabled (gate needs both).
	store.SetSetting("booknlp_enabled", "true")
	if _, out := getCast(); out["enabled"] != false {
		t.Errorf("enabled = %v with flag-on/no-service, want false", out["enabled"])
	}
	// Flag on AND service configured → enabled.
	srv.BookNLPURL = "http://localhost:5300"
	if _, out := getCast(); out["enabled"] != true {
		t.Errorf("enabled = %v with flag-on + service, want true", out["enabled"])
	}

	// Populated cast → rows carry name + aliases + gender + mention_count.
	store.ReplaceCharactersForBook(workID, bookID, []db.Character{
		{Name: "Elizabeth Bennet", Aliases: []string{"Lizzy", "Eliza"}, Gender: "she/her", MentionCount: 142},
		{Name: "Mr. Darcy", Aliases: []string{"Darcy"}, Gender: "he/him/his", MentionCount: 98},
	})
	code, out = getCast()
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	chars, _ := out["characters"].([]any)
	if len(chars) != 2 {
		t.Fatalf("characters = %d, want 2", len(chars))
	}
	first, _ := chars[0].(map[string]any)
	if first["name"] != "Elizabeth Bennet" {
		t.Errorf("first name = %v, want Elizabeth Bennet (ranked by mentions)", first["name"])
	}
	if first["mention_count"].(float64) != 142 {
		t.Errorf("mention_count = %v, want 142", first["mention_count"])
	}
	if al, ok := first["aliases"].([]any); !ok || len(al) != 2 {
		t.Errorf("aliases = %v, want 2", first["aliases"])
	}
}

// A work with no EPUB text source is a foreseeable input condition → graceful
// 422, never a bare 500 (even with BookNLP configured).
func TestHandleExtractCastNoEPUB(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.BookNLPURL = "http://127.0.0.1:5300" // configured, but never reached
	store.SetSetting("booknlp_enabled", "true")
	workID, _ := store.CreateWork("Audio Only", "Author") // no text book at all

	req := httptest.NewRequest("POST", "/api/works/x/extract-cast", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleExtractCast(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (graceful, not 500)", rec.Code)
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if strings.Contains(out["error"], "internal server error") || out["error"] == "" {
		t.Errorf("not a clear graceful message: %q", out["error"])
	}
}

// #133: an invalid work id is a 400, not a panic.
func TestHandleGetCastInvalidID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/works/x/cast", nil)
	req.SetPathValue("id", "not-a-number")
	rec := httptest.NewRecorder()
	srv.handleGetCast(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
