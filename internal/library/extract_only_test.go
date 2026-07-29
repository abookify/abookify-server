package library

import (
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// TestExtractOnly_NeverLeaksAheadOfPosition is the belt-and-braces spoiler test
// the whole mode exists for: a memorized classic (Frankenstein) where a LATE
// chapter holds a plot spoiler the model "knows" but the reader hasn't reached.
// Extract-only must answer ONLY from what's been read — and, being no-generation,
// it runs with rag=nil (no LLM at all), so nothing can inject the spoiler.
func TestExtractOnly_NeverLeaksAheadOfPosition(t *testing.T) {
	store := testStoreForLib(t)

	if err := store.UpsertBook(db.Book{
		Path: "/f/frankenstein.epub", Filename: "frankenstein.epub",
		Format: "epub", MediaType: "text", Title: "Frankenstein",
	}); err != nil {
		t.Fatal(err)
	}
	var bookID int64
	books, _ := store.ListBooks()
	for _, b := range books {
		if b.Path == "/f/frankenstein.epub" {
			bookID = b.ID
		}
	}
	workID, err := store.CreateWork("Frankenstein", "Mary Shelley")
	if err != nil {
		t.Fatal(err)
	}
	store.AssignBooksToWork(workID, []int64{bookID})

	// Chapters the reader has read (0,1) + a late chapter (19) with the spoiler.
	for _, ci := range []int{0, 1, 19} {
		store.InsertChapter(db.Chapter{BookID: bookID, Index: ci, Title: "Chapter", WordCount: 20})
	}
	store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 0, ChunkIdx: 0,
		Content: "I am by birth a Genevese, and my family is one of the most distinguished of that republic."})
	store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 1, ChunkIdx: 0,
		Content: "I entered the university of Ingolstadt to study natural philosophy and chemistry."})
	// THE SPOILER — chapter 19, must never surface for a reader at chapter 1.
	const spoiler = "the creature strangled little William, my youngest brother"
	store.InsertChunk(db.Chunk{BookID: bookID, ChapterIdx: 19, ChunkIdx: 0,
		Content: "In the dark woods " + spoiler + " to take revenge on his maker."})

	// Reader is at chapter 1. The keyword ("William") DOES match the chapter-19
	// spoiler chunk, so keyword retrieval would surface it — this only stays safe
	// because the reading scope filters chapter 19 back out. That's the real test.
	scope := ResolveSessionScope("reading", bookID, 1, QueryScope{})
	ans, err := AskInSession(store, nil /* NO LLM */, workID, nil, "William", scope, true /* extractOnly */)
	if err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(ans.Text)
	if strings.Contains(low, "strangled") || strings.Contains(low, "creature") || strings.Contains(low, "revenge") {
		t.Fatalf("extract-only LEAKED ahead-of-position content:\n%s", ans.Text)
	}
	if ans.Model != "extract-only" {
		t.Errorf("expected extract-only marker, got model=%q", ans.Model)
	}

	// Whole-book mode (opt-in) is allowed to surface the spoiler — that's the
	// contrast that proves the scope, not the mode, is what was hiding it. Still
	// extract-only (no generation): the text is the book's, verbatim.
	whole, err := AskInSession(store, nil, workID, nil, "William", QueryScope{Type: "book"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(whole.Text), "strangled") {
		t.Errorf("whole-book extract should include the passage that mentions William; got:\n%s", whole.Text)
	}
}

// TestComposeExtractAnswer_VerbatimOnly proves the composer emits ONLY the
// passages handed to it (no fabrication), so its output can never exceed what
// retrieval already bounded to the reading position.
func TestComposeExtractAnswer_VerbatimOnly(t *testing.T) {
	getTitle := func(_ int64, ch int) string { return "Ch" }
	got := composeExtractAnswer([]db.Chunk{
		{BookID: 1, ChapterIdx: 0, Content: "alpha passage"},
		{BookID: 1, ChapterIdx: 0, Content: "beta passage"},
	}, nil, getTitle)
	if !strings.Contains(got.Text, "alpha passage") || !strings.Contains(got.Text, "beta passage") {
		t.Errorf("composed answer dropped a passage: %q", got.Text)
	}
	if strings.Contains(got.Text, "gamma") {
		t.Errorf("composed answer fabricated content not in the passages: %q", got.Text)
	}
	// Empty → honest, no-content reply (never invents an answer).
	empty := composeExtractAnswer(nil, nil, getTitle)
	if !strings.Contains(strings.ToLower(empty.Text), "hasn't come up") {
		t.Errorf("empty extract should be the honest fallback, got %q", empty.Text)
	}
}
