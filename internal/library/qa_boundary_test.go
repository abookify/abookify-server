package library

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

// TestQAOutboundBoundary_MinimalSend enforces the privacy stance in code: a Q&A
// question sends ONLY the retrieved passages + the question to the LLM — never
// the whole book. It captures the ACTUAL outbound request and asserts the
// non-retrieved book text does not leave the machine. If a future change widens
// the prompt to include un-retrieved content, this test fails.
func TestQAOutboundBoundary_MinimalSend(t *testing.T) {
	store := testStoreForLib(t)

	workID, err := store.CreateWork("Boundary Book", "Ada Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	textPath := filepath.Join(t.TempDir(), "book.epub")
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: textPath, Filename: "book.epub",
		Format: "epub", MediaType: "text", Title: "Boundary Book", Origin: "publisher_epub",
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

	// One chunk answers the question (unique keyword). The others are the rest of
	// the "book" — unique sentinels that must never leave.
	if err := store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 0, ChunkIdx: 0, Content: "The zorblatt engine glows a soft blue when it runs.", StartWord: 0, EndWord: 10}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 0, ChunkIdx: 1, Content: "HIDDENTREASURE is buried beneath the old oak by the mill.", StartWord: 11, EndWord: 20})
	store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 1, ChunkIdx: 2, Content: "SPOILERENDING reveals the butler did it the whole time.", StartWord: 21, EndWord: 30})

	// Capture server: record the chat request body; 500 on anything else (so a
	// vector-search query-embed falls through to keyword retrieval).
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			b, _ := io.ReadAll(r.Body)
			captured = string(b)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "The zorblatt engine glows blue."}}},
				"model":   "gpt-4o",
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := llm.NewClient(llm.ProviderOpenAI, "test-key", "gpt-4o", srv.URL)
	rag := llm.NewRAG(store, client)

	ans, err := AskWithCitations(store, rag, workID, "What is the zorblatt engine?", QueryScope{Type: "book"}, false)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if ans == nil || captured == "" {
		t.Fatalf("no outbound request captured (ans=%v)", ans)
	}

	// The retrieved passage + the question DID leave (that's the minimum needed).
	if !strings.Contains(captured, "zorblatt engine glows") {
		t.Fatalf("retrieved passage missing from outbound body:\n%s", captured)
	}
	if !strings.Contains(captured, "What is the zorblatt engine") {
		t.Fatalf("question missing from outbound body")
	}
	// The REST of the book did NOT leave — the enforced boundary.
	for _, leak := range []string{"HIDDENTREASURE", "SPOILERENDING"} {
		if strings.Contains(captured, leak) {
			t.Fatalf("PRIVACY BOUNDARY VIOLATED: non-retrieved book text %q left the machine:\n%s", leak, captured)
		}
	}
}
