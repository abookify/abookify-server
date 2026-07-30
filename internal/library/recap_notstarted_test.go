package library

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

// TestAskInSession_NotStartedBook: a fresh chat (no history) asking within a
// reading scope pinned at the start, with nothing read yet, gets the honest
// "you haven't started" recap message — NOT the generic "hasn't come up yet"
// decline (which reads as topic-not-found) and NOT a model guess. This is the
// first-tap case every new user hits (BUG: PJ on Hitchhiker's at position 0).
func TestAskInSession_NotStartedBook(t *testing.T) {
	store := testStoreForLib(t)
	workID, _ := store.CreateWork("Unstarted Book", "Ada Author")
	textPath := filepath.Join(t.TempDir(), "book.epub")
	store.UpsertBook(db.Book{WorkID: workID, Path: textPath, Filename: "book.epub", Format: "epub", MediaType: "text", Title: "Unstarted Book", Origin: "publisher_epub"})
	var bookID int64
	for _, b := range mustBooks(store) {
		if b.Path == textPath {
			bookID = b.ID
		}
	}
	// Content exists only from chapter 3 on — so a reader at the start (up_to
	// chapter 0) has nothing in scope.
	store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 3, ChunkIdx: 0, Content: "Far later in the story, things happen.", StartWord: 0, EndWord: 8})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	rag := llm.NewRAG(store, llm.NewClient(llm.ProviderOpenAI, "k", "gpt-4o", srv.URL))

	scope := ResolveSessionScope("reading", bookID, 0, QueryScope{}) // reader at the very start
	ans, err := AskInSession(store, rag, workID, nil /*no history*/, "summarize what has happened so far", scope, false)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if ans.Text != NotStartedRecapMessage {
		t.Fatalf("expected the not-started message, got: %q", ans.Text)
	}
}

func mustBooks(store *db.Store) []db.Book { bs, _ := store.ListBooks(); return bs }
