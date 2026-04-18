package library

import (
	"path/filepath"
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
