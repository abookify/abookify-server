package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// Extract-only Q&A must serve with NO LLM key: it returns the book's own
// retrieved, position-bounded passages VERBATIM and never calls a model. The
// /ask handler used to 503 on a nil RAG before checking extract-only, which
// denied the strongest-guarantee mode to exactly the people with no key. This
// locks the fix: no key + extract-only → 200 with the book's own words, not 503.
func TestAskExtractOnlyServesWithNoLLMKey(t *testing.T) {
	srv, store, _ := newTestServer(t) // New(store,"0") wires no LLM → RAG() is nil
	if srv.RAG() != nil {
		t.Skip("test assumes no LLM is configured")
	}
	if err := store.SetSetting("qa_extract_only", "true"); err != nil {
		t.Fatal(err)
	}
	wid, err := store.CreateWork("Frankenstein", "Mary Shelley")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBook(db.Book{WorkID: wid, Path: "/f.epub", Filename: "f.epub", Format: "epub", MediaType: "text", Title: "Frankenstein"}); err != nil {
		t.Fatal(err)
	}
	wk, _ := store.GetWork(wid)
	if wk == nil || len(wk.TextFiles) == 0 {
		t.Fatalf("expected a text book on the work")
	}
	bid := wk.TextFiles[0].ID
	passage := "It was on a dreary night of November that I beheld the accomplishment of my toils."
	store.InsertChapter(db.Chapter{BookID: bid, Index: 0, Title: "Chapter 1", Content: passage, WordCount: 15})
	store.InsertChunk(db.Chunk{BookID: bid, ChapterIdx: 0, ChunkIdx: 0, Content: passage, StartWord: 0, EndWord: 15})

	req := httptest.NewRequest("POST", "/api/works/x/ask", strings.NewReader(`{"question":"what did i behold on that dreary november night of accomplishment"}`))
	req.SetPathValue("id", fmt.Sprint(wid))
	rec := httptest.NewRecorder()
	srv.handleAskQuestion(rec, req)

	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("extract-only Q&A 503'd with NO LLM key — the mobile no-key block: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dreary night of November") {
		t.Errorf("extract-only must return the book's own passage verbatim; got: %s", rec.Body.String())
	}
}
