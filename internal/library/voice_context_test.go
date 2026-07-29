package library

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

// seedVoiceWork builds a work with a safe early chunk (chapter 0) and a spoiler
// chunk (chapter 5), plus a keyword-only RAG (its embed endpoint 500s, so
// retrieval falls through to keyword search — no live API call).
func seedVoiceWork(t *testing.T) (*db.Store, int64, int64, *llm.RAG) {
	t.Helper()
	store := testStoreForLib(t)
	workID, err := store.CreateWork("Voice Book", "Ada Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	textPath := filepath.Join(t.TempDir(), "book.epub")
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: textPath, Filename: "book.epub",
		Format: "epub", MediaType: "text", Title: "Voice Book", Origin: "publisher_epub",
	}); err != nil {
		t.Fatalf("upsert book: %v", err)
	}
	var bookID int64
	books, _ := store.ListBooks()
	for _, b := range books {
		if b.Path == textPath {
			bookID = b.ID
		}
	}
	store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 0, ChunkIdx: 0, Content: "The zorblatt engine glows a soft blue when the butler starts it.", StartWord: 0, EndWord: 12})
	store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 5, ChunkIdx: 1, Content: "SPOILERENDING: the butler was the murderer all along.", StartWord: 13, EndWord: 22})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // embed fails → keyword retrieval
	}))
	t.Cleanup(srv.Close)
	rag := llm.NewRAG(store, llm.NewClient(llm.ProviderOpenAI, "test-key", "gpt-4o", srv.URL))
	return store, workID, bookID, rag
}

func containsPassage(ps []VoiceContextPassage, needle string) bool {
	for _, p := range ps {
		if strings.Contains(p.Content, needle) {
			return true
		}
	}
	return false
}

// TestVoiceContext_BoundedToReadingPosition is the core guarantee: asking ALOUD
// is bounded exactly like typing. A question whose answer is a chapter-5 spoiler
// surfaces it under whole-book scope, but NEVER under a "reading" scope bounded
// to chapter 2 — the voice retrieval tool can't leak what the reader hasn't read.
func TestVoiceContext_BoundedToReadingPosition(t *testing.T) {
	store, workID, bookID, rag := seedVoiceWork(t)

	// Whole-book scope surfaces the spoiler (proves the query finds it, so the
	// bound below is doing real work — not just failing to retrieve).
	whole, err := VoiceContext(store, rag, workID, "who was the murderer butler", QueryScope{Type: "book"}, false)
	if err != nil {
		t.Fatalf("whole-book VoiceContext: %v", err)
	}
	if !whole.Grounded || !containsPassage(whole.Passages, "SPOILERENDING") {
		t.Fatalf("whole-book scope should surface the spoiler chunk, got %+v", whole.Passages)
	}

	// Reading scope bounded to chapter 2: the SAME question must NOT return the
	// chapter-5 spoiler.
	reading := ResolveSessionScope("reading", bookID, 2, QueryScope{})
	bounded, err := VoiceContext(store, rag, workID, "who was the murderer butler", reading, false)
	if err != nil {
		t.Fatalf("reading-scope VoiceContext: %v", err)
	}
	if !bounded.Grounded {
		t.Fatal("reading scope should still ground (in-scope passages exist)")
	}
	if containsPassage(bounded.Passages, "SPOILERENDING") {
		t.Fatalf("SPOILER LEAK: voice returned chapter-5 content to a reader at chapter 2:\n%+v", bounded.Passages)
	}
}

// TestVoiceContext_ExtractOnlyDeclines: with extract-only (spoiler-safe) on,
// voice book-grounding declines — no book content is fed to the generative voice
// model, so it can't leak what text Q&A (verbatim passages) wouldn't.
func TestVoiceContext_ExtractOnlyDeclines(t *testing.T) {
	store, workID, _, rag := seedVoiceWork(t)
	res, err := VoiceContext(store, rag, workID, "who was the murderer", QueryScope{Type: "book"}, true)
	if err != nil {
		t.Fatalf("extract-only VoiceContext: %v", err)
	}
	if res.Grounded || len(res.Passages) != 0 {
		t.Fatalf("extract-only must decline grounding (no passages), got %+v", res)
	}
	if res.Reason == "" {
		t.Fatal("extract-only decline should carry a reason for the UI")
	}
}
