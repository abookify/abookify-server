package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// Extraction runs in-process, so the whole path — EPUB chapters in, stored cast
// out — is exercisable without any service, flag or network. This is what
// replaced the old "BookNLP is opt-in and probably not running, so fail soft
// with a 503" contract: there is nothing left to be down.
func TestHandleExtractCastInProcess(t *testing.T) {
	srv, store, dir := newTestServer(t)

	workID, _ := store.CreateWork("Frankenstein", "Shelley")
	epub := dir + "/f.epub"
	store.UpsertBook(db.Book{WorkID: workID, Path: epub, Filename: "f.epub",
		Format: "epub", MediaType: "text", Title: "Frankenstein", Origin: "publisher_epub"})
	bookID := bookIDByPath(t, store, epub)

	// Mid-sentence capitals are the name signal; a sentence-initial common word
	// must not become a character.
	// Vary the wording: a name that always sits directly after the same
	// sentence-initial word would legitimately merge with it as a bigram.
	body := strings.Repeat(
		"I walked with Clerval through the town. Then Clerval spoke to Elizabeth. "+
			"The letter from Elizabeth reached Clerval in autumn. "+
			"Later that winter Elizabeth wrote to Clerval again. ", 6)
	store.InsertChapter(db.Chapter{BookID: bookID, Index: 0, Title: "Ch1",
		Content: body, WordCount: len(strings.Fields(body))})

	req := httptest.NewRequest("POST", "/api/works/x/extract-cast", nil)
	req.SetPathValue("id", itoa(workID))
	rec := httptest.NewRecorder()
	srv.handleExtractCast(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if n, _ := out["characters"].(float64); n < 2 {
		t.Fatalf("characters = %v, want >=2 (Clerval, Elizabeth)", out["characters"])
	}

	chars, err := store.ListCharactersForWork(workID)
	if err != nil {
		t.Fatalf("list characters: %v", err)
	}
	found := map[string]bool{}
	for _, c := range chars {
		found[c.Name] = true
		if c.MentionCount <= 0 {
			t.Errorf("%q stored with mention_count %d", c.Name, c.MentionCount)
		}
	}
	for _, want := range []string{"Clerval", "Elizabeth"} {
		if !found[want] {
			t.Errorf("%q missing from stored cast; got %v", want, found)
		}
	}
	if found["Then"] || found["Later"] {
		t.Errorf("sentence-initial word stored as a character: %v", found)
	}
}

// The cast endpoint always reports experimental:true — the badge is mandatory
// on every surface — reports enabled:true now that the extractor is in-process,
// and returns an empty (never null) list when there is no cast.
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

	code, out := getCast()
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out["experimental"] != true {
		t.Errorf("experimental = %v, want true (mandatory on every cast surface)", out["experimental"])
	}
	if out["enabled"] != true {
		t.Errorf("enabled = %v, want true (in-process — nothing to enable)", out["enabled"])
	}
	if chars, ok := out["characters"].([]any); !ok || len(chars) != 0 {
		t.Errorf("characters = %v, want [] (empty, not null)", out["characters"])
	}

	// Populated cast → rows carry name + aliases + mention_count, ranked.
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
}

// A work with no EPUB text source is a foreseeable input condition → graceful
// 422, never a bare 500.
func TestHandleExtractCastNoEPUB(t *testing.T) {
	srv, store, _ := newTestServer(t)
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

// An invalid work id is a 400, not a panic.
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
