package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

// fakeLLM is an httptest OpenAI-compatible chat endpoint. It records every
// request body it sees (so a test can prove which chapter text reached the
// model — the spoiler bound) and returns a canned completion.
type fakeLLM struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	reply  string
}

func newFakeLLM(t *testing.T, reply string) *fakeLLM {
	t.Helper()
	f := &fakeLLM{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":   "gpt-test",
			"choices": []map[string]any{{"message": map[string]string{"content": f.reply}}},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// sawMarker reports whether any captured request body contained s.
func (f *fakeLLM) sawMarker(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bodies {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

func (f *fakeLLM) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.bodies) }

// wireLLM points the server's RAG at the fake OpenAI endpoint.
func wireLLM(srv *Server, f *fakeLLM) {
	client := llm.NewClient(llm.ProviderOpenAI, "test-key", "gpt-test", f.srv.URL)
	srv.rag.Store(llm.NewRAG(srv.store, client))
}

// seedTextChapters inserts a text book with n chapters, each carrying a unique
// marker token + enough words to clear the summarize threshold (≥20).
func seedTextChapters(t *testing.T, store *db.Store, n int) (workID, bookID int64) {
	t.Helper()
	workID, err := store.CreateWork("Recap Book", "Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: fmt.Sprintf("/tmp/recap-%d.epub", workID), Filename: "r.epub",
		Format: "epub", MediaType: "text", Title: "Recap Book", Origin: "publisher_epub",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	bookID = bookIDByPath(t, store, fmt.Sprintf("/tmp/recap-%d.epub", workID))
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("CHAPMARK_%d %s", i, strings.Repeat("word ", 30))
		store.InsertChapter(db.Chapter{
			BookID: bookID, Index: i, Title: fmt.Sprintf("Chapter %d", i+1),
			Content: body, WordCount: 31,
		})
	}
	return workID, bookID
}

// #134: every summary route is hard-gated on a configured LLM — no key, 503.
func TestSummariesGate503(t *testing.T) {
	srv, store, _ := newTestServer(t)
	_, bookID := seedTextChapters(t, store, 2)

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		path    string
		vals    map[string]string
		query   string
	}{
		{"chapter", srv.handleChapterSummary, "/api/books/x/chapters/0/summary",
			map[string]string{"bookId": itoa(bookID), "idx": "0"}, ""},
		{"recap", srv.handleBookRecap, "/api/books/x/recap",
			map[string]string{"bookId": itoa(bookID)}, "up_to=1"},
	} {
		req := httptest.NewRequest("GET", tc.path+"?"+tc.query, nil)
		for k, v := range tc.vals {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		tc.handler(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 (no LLM)", tc.name, rec.Code)
		}
	}
}

// #134: a chapter summary generates once, then serves from cache; a too-short
// chapter summarizes to "" without calling the model.
func TestChapterSummaryCacheAndShort(t *testing.T) {
	srv, store, _ := newTestServer(t)
	f := newFakeLLM(t, "A concise chapter summary.")
	wireLLM(srv, f)
	_, bookID := seedTextChapters(t, store, 1)
	// A deliberately short chapter (under the 20-word floor).
	store.InsertChapter(db.Chapter{BookID: bookID, Index: 5, Title: "Title Page", Content: "Short.", WordCount: 1})

	call := func(idx int, refresh bool) (string, bool) {
		t.Helper()
		q := ""
		if refresh {
			q = "?refresh=1"
		}
		req := httptest.NewRequest("GET", "/api/books/x/chapters/y/summary"+q, nil)
		req.SetPathValue("bookId", itoa(bookID))
		req.SetPathValue("idx", itoa(int64(idx)))
		rec := httptest.NewRecorder()
		srv.handleChapterSummary(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Summary string `json:"summary"`
			Cached  bool   `json:"cached"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Summary, out.Cached
	}

	if sum, cached := call(0, false); sum == "" || cached {
		t.Errorf("first call = (%q, cached=%v), want non-empty + cached=false", sum, cached)
	}
	callsAfterGen := f.calls()
	if sum, cached := call(0, false); sum == "" || !cached {
		t.Errorf("second call = (%q, cached=%v), want cached=true", sum, cached)
	}
	if f.calls() != callsAfterGen {
		t.Errorf("cache hit still called the model (%d → %d)", callsAfterGen, f.calls())
	}
	// Short chapter → empty summary, no model call.
	before := f.calls()
	if sum, _ := call(5, false); sum != "" {
		t.Errorf("short chapter summary = %q, want \"\"", sum)
	}
	if f.calls() != before {
		t.Errorf("short chapter hit the model (%d → %d)", before, f.calls())
	}
}

// #134/#130: the recap is bounded to up_to — chapters past the reader's
// position are NEVER sent to the model, and the recap caches.
func TestBookRecapSpoilerBound(t *testing.T) {
	srv, store, _ := newTestServer(t)
	f := newFakeLLM(t, "Recap text so far.")
	wireLLM(srv, f)
	_, bookID := seedTextChapters(t, store, 6) // chapters 0..5

	get := func(upTo int) (string, bool) {
		t.Helper()
		req := httptest.NewRequest("GET", fmt.Sprintf("/api/books/x/recap?up_to=%d", upTo), nil)
		req.SetPathValue("bookId", itoa(bookID))
		rec := httptest.NewRecorder()
		srv.handleBookRecap(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Recap  string `json:"recap"`
			Cached bool   `json:"cached"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Recap, out.Cached
	}

	recap, cached := get(2)
	if recap == "" || cached {
		t.Errorf("recap = (%q, cached=%v), want non-empty + cached=false", recap, cached)
	}
	// The spoiler bound: chapters 0..2 reached the model, 3..5 never did.
	for _, i := range []int{0, 1, 2} {
		if !f.sawMarker(fmt.Sprintf("CHAPMARK_%d", i)) {
			t.Errorf("chapter %d should have been summarized (≤ up_to)", i)
		}
	}
	for _, i := range []int{3, 4, 5} {
		if f.sawMarker(fmt.Sprintf("CHAPMARK_%d", i)) {
			t.Errorf("SPOILER LEAK: chapter %d (> up_to=2) reached the model", i)
		}
	}
	// Only chapters ≤ up_to are cached in the summaries table.
	for _, i := range []int{0, 1, 2} {
		if _, _, ok, _ := store.GetSummary(bookID, "chapter", i); !ok {
			t.Errorf("chapter %d summary should be cached", i)
		}
	}
	for _, i := range []int{3, 4, 5} {
		if _, _, ok, _ := store.GetSummary(bookID, "chapter", i); ok {
			t.Errorf("chapter %d (> up_to) must not be cached", i)
		}
	}

	// Second recap at the same bound is served from cache (no new model calls).
	before := f.calls()
	if r2, c2 := get(2); r2 == "" || !c2 {
		t.Errorf("cached recap = (%q, cached=%v), want cached=true", r2, c2)
	}
	if f.calls() != before {
		t.Errorf("cached recap still called the model (%d → %d)", before, f.calls())
	}
}

// #134: malformed params are rejected (once past the LLM gate).
func TestBookRecapBadParams(t *testing.T) {
	srv, store, _ := newTestServer(t)
	wireLLM(srv, newFakeLLM(t, "x"))
	_, bookID := seedTextChapters(t, store, 1)

	req := httptest.NewRequest("GET", "/api/books/x/recap", nil) // no up_to
	req.SetPathValue("bookId", itoa(bookID))
	rec := httptest.NewRecorder()
	srv.handleBookRecap(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing up_to: status = %d, want 400", rec.Code)
	}
}
