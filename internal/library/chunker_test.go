package library

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

func testStoreForLib(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestChunkBook(t *testing.T) {
	store := testStoreForLib(t)

	// Create a book with chapters
	store.UpsertBook(db.Book{Path: "/test.epub", Filename: "test.epub", Format: "epub", MediaType: "text"})
	books, _ := store.ListBooks()
	bookID := books[0].ID

	// Insert a chapter with enough content to create multiple chunks
	words := make([]string, 500)
	for i := range words {
		words[i] = "word"
	}
	content := ""
	for i, w := range words {
		if i > 0 {
			content += " "
		}
		content += w
	}

	store.InsertChapter(db.Chapter{
		BookID:    bookID,
		Index:     0,
		Title:     "Chapter 1",
		Content:   content,
		WordCount: 500,
	})

	if err := ChunkBook(store, bookID); err != nil {
		t.Fatalf("chunk: %v", err)
	}

	count, _ := store.ChunkCount(bookID)
	// 500 words / (200-40) stride = ~3.1, so 4 chunks
	if count < 3 || count > 5 {
		t.Errorf("chunk count: got %d, expected 3-5", count)
	}

	// Should not re-chunk
	if err := ChunkBook(store, bookID); err != nil {
		t.Fatalf("re-chunk: %v", err)
	}
	count2, _ := store.ChunkCount(bookID)
	if count2 != count {
		t.Errorf("re-chunk changed count: %d -> %d", count, count2)
	}
}

func TestMatcherNormalize(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Frankenstein; or, The Modern Prometheus", "frankenstein or the modern prometheus"},
		{"Pride and Prejudice", "pride and prejudice"},
		{"Dr. Jekyll & Mr. Hyde", "dr jekyll mr hyde"},
	}

	for _, tt := range tests {
		got := normalize(tt.input)
		if got != tt.expected {
			t.Errorf("normalize(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestOverlapScore(t *testing.T) {
	// Words > 2 chars overlap
	score := overlapScore("frankenstein modern prometheus", "frankenstein modern prometheus shelley")
	if score != 3 {
		t.Errorf("full overlap: got %d, want 3", score)
	}

	score = overlapScore("pride prejudice", "war worlds")
	if score != 0 {
		t.Errorf("no overlap: got %d, want 0", score)
	}
}

// words builds n space-separated tokens — enough content to produce several
// chunks per chapter.
func chunkTestWords(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = "word"
	}
	return strings.Join(out, " ")
}

// A re-split transcript renumbers every chapter, so chunks keyed by the OLD
// chapter_idx both under-cover the book and cite the wrong chapter. ChunkBook
// must notice and rebuild rather than short-circuit on "chunks exist".
func TestChunkBookRebuildsAfterResplit(t *testing.T) {
	store := testStoreForLib(t)
	store.UpsertBook(db.Book{Path: "/t.txt", Filename: "t.txt", Format: "transcript", MediaType: "text"})
	books, _ := store.ListBooks()
	bookID := books[0].ID

	// Original segmentation: 2 chapters.
	for i := 0; i < 2; i++ {
		store.InsertChapter(db.Chapter{BookID: bookID, Index: i, Title: "seg",
			Content: chunkTestWords(500), WordCount: 500})
	}
	if err := ChunkBook(store, bookID); err != nil {
		t.Fatalf("initial chunk: %v", err)
	}
	before, _ := store.ChunkCount(bookID)

	// Re-split to 5 chapters — the same words under a different segmentation.
	store.DeleteChaptersByBook(bookID)
	for i := 0; i < 5; i++ {
		store.InsertChapter(db.Chapter{BookID: bookID, Index: i, Title: "seg",
			Content: chunkTestWords(200), WordCount: 200})
	}
	if err := ChunkBook(store, bookID); err != nil {
		t.Fatalf("rechunk: %v", err)
	}

	chunks, _ := store.ListChunks(bookID)
	if len(chunks) == before {
		t.Errorf("chunk count unchanged at %d after re-split — stale chunks kept", before)
	}
	idx := map[int]bool{}
	for _, c := range chunks {
		idx[c.ChapterIdx] = true
	}
	if len(idx) != 5 {
		t.Errorf("chunks cover %d chapter(s), want 5 — chapter_idx still from the old split", len(idx))
	}
	for i := 0; i < 5; i++ {
		if !idx[i] {
			t.Errorf("chapter %d has no chunks", i)
		}
	}
}

// A re-transcription keeps the chapter count but grows the words behind it (a
// repaired source file recovering lost narration). Chunks that stop far short
// of the chapter's text are stale even though every chapter index is present.
func TestChunkBookRebuildsAfterContentGrows(t *testing.T) {
	store := testStoreForLib(t)
	store.UpsertBook(db.Book{Path: "/g.txt", Filename: "g.txt", Format: "transcript", MediaType: "text"})
	books, _ := store.ListBooks()
	bookID := books[0].ID

	store.InsertChapter(db.Chapter{BookID: bookID, Index: 0, Title: "seg",
		Content: chunkTestWords(300), WordCount: 300})
	if err := ChunkBook(store, bookID); err != nil {
		t.Fatalf("initial chunk: %v", err)
	}
	before, _ := store.ChunkCount(bookID)

	// Same single chapter, four times the narration.
	store.DeleteChaptersByBook(bookID)
	store.InsertChapter(db.Chapter{BookID: bookID, Index: 0, Title: "seg",
		Content: chunkTestWords(1200), WordCount: 1200})
	if err := ChunkBook(store, bookID); err != nil {
		t.Fatalf("rechunk: %v", err)
	}

	after, _ := store.ChunkCount(bookID)
	if after <= before {
		t.Errorf("chunks %d -> %d: recovered narration never got chunked", before, after)
	}
	chunks, _ := store.ListChunks(bookID)
	maxEnd := 0
	for _, c := range chunks {
		if c.EndWord > maxEnd {
			maxEnd = c.EndWord
		}
	}
	if maxEnd < 1200 {
		t.Errorf("chunks reach word %d of 1200 — tail of the chapter unsearchable", maxEnd)
	}
}

// The guard must not fire on an unchanged book: rebuilding means re-embedding,
// which is the expensive half of the pipeline.
func TestChunkBookStableWhenUnchanged(t *testing.T) {
	store := testStoreForLib(t)
	store.UpsertBook(db.Book{Path: "/s.txt", Filename: "s.txt", Format: "transcript", MediaType: "text"})
	books, _ := store.ListBooks()
	bookID := books[0].ID

	for i := 0; i < 3; i++ {
		store.InsertChapter(db.Chapter{BookID: bookID, Index: i, Title: "seg",
			Content: chunkTestWords(450), WordCount: 450})
	}
	if err := ChunkBook(store, bookID); err != nil {
		t.Fatalf("initial chunk: %v", err)
	}
	chunks, _ := store.ListChunks(bookID)
	firstID := chunks[0].ID

	for i := 0; i < 3; i++ {
		if err := ChunkBook(store, bookID); err != nil {
			t.Fatalf("re-chunk: %v", err)
		}
	}
	again, _ := store.ListChunks(bookID)
	if len(again) != len(chunks) {
		t.Errorf("chunk count drifted %d -> %d on an unchanged book", len(chunks), len(again))
	}
	// A rebuild would delete and re-insert, moving the autoincrement id.
	if again[0].ID != firstID {
		t.Errorf("chunks were rebuilt (id %d -> %d) — unchanged book re-embedded", firstID, again[0].ID)
	}
}
