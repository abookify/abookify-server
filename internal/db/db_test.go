package db

import (
	"os"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestOpenAndMigrate(t *testing.T) {
	store := testStore(t)
	if store == nil {
		t.Fatal("store is nil")
	}
}

func TestUpsertAndListBooks(t *testing.T) {
	store := testStore(t)

	book := Book{
		Path:      "/test/book.epub",
		Filename:  "book.epub",
		Format:    "epub",
		MediaType: "text",
		SizeBytes: 1234,
		Title:     "Test Book",
		Author:    "Test Author",
	}

	if err := store.UpsertBook(book); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	books, err := store.ListBooks()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(books) != 1 {
		t.Fatalf("expected 1 book, got %d", len(books))
	}

	if books[0].Title != "Test Book" {
		t.Errorf("title = %q, want %q", books[0].Title, "Test Book")
	}

	// Upsert same path — should update, not duplicate
	book.Title = "Updated Title"
	store.UpsertBook(book)

	books, _ = store.ListBooks()
	if len(books) != 1 {
		t.Fatalf("upsert created duplicate: got %d books", len(books))
	}
	if books[0].Title != "Updated Title" {
		t.Errorf("title not updated: %q", books[0].Title)
	}
}

func TestWorksLifecycle(t *testing.T) {
	store := testStore(t)

	// Create work
	workID, err := store.CreateWork("Frankenstein", "Mary Shelley")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if workID == 0 {
		t.Fatal("work ID is 0")
	}

	// Add books
	store.UpsertBook(Book{Path: "/audio/ch1.mp3", Filename: "ch1.mp3", Format: "mp3", MediaType: "audio", Title: "Chapter 1"})
	store.UpsertBook(Book{Path: "/text/book.epub", Filename: "book.epub", Format: "epub", MediaType: "text", Title: "Frankenstein"})

	books, _ := store.ListBooks()
	ids := make([]int64, len(books))
	for i, b := range books {
		ids[i] = b.ID
	}
	store.AssignBooksToWork(workID, ids)

	// Get work with books
	work, err := store.GetWork(workID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if !work.HasAudio {
		t.Error("work should have audio")
	}
	if !work.HasText {
		t.Error("work should have text")
	}
	if len(work.AudioFiles) != 1 {
		t.Errorf("audio files: got %d, want 1", len(work.AudioFiles))
	}
	if len(work.TextFiles) != 1 {
		t.Errorf("text files: got %d, want 1", len(work.TextFiles))
	}
}

func TestChapters(t *testing.T) {
	store := testStore(t)
	store.UpsertBook(Book{Path: "/test.epub", Filename: "test.epub", Format: "epub", MediaType: "text"})
	books, _ := store.ListBooks()
	bookID := books[0].ID

	// Insert chapters
	for i := 0; i < 5; i++ {
		store.InsertChapter(Chapter{
			BookID:    bookID,
			Index:     i,
			Title:     "Chapter",
			Content:   "Some content here",
			WordCount: 3,
		})
	}

	count, _ := store.ChapterCount(bookID)
	if count != 5 {
		t.Errorf("chapter count: got %d, want 5", count)
	}

	chapters, _ := store.ListChapters(bookID)
	if len(chapters) != 5 {
		t.Errorf("list chapters: got %d, want 5", len(chapters))
	}

	ch, _ := store.GetChapterContent(bookID, 2)
	if ch == nil {
		t.Fatal("chapter 2 is nil")
	}
	if ch.Content != "Some content here" {
		t.Errorf("content = %q", ch.Content)
	}
}

func TestPlaybackPositions(t *testing.T) {
	store := testStore(t)

	pos := PlaybackPosition{WorkID: 1, BookID: 10, FileIndex: 3, PositionSecs: 45.5}
	if err := store.SavePosition(pos); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.GetPosition(1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PositionSecs != 45.5 {
		t.Errorf("position = %f, want 45.5", got.PositionSecs)
	}
	if got.FileIndex != 3 {
		t.Errorf("file_index = %d, want 3", got.FileIndex)
	}

	// Update
	pos.PositionSecs = 90.0
	store.SavePosition(pos)
	got, _ = store.GetPosition(1)
	if got.PositionSecs != 90.0 {
		t.Errorf("updated position = %f, want 90.0", got.PositionSecs)
	}
}

func TestBookmarks(t *testing.T) {
	store := testStore(t)

	id, err := store.CreateBookmark(Bookmark{
		WorkID: 1, BookID: 10, Type: "bookmark",
		ChapterIdx: 5, PositionSecs: 120.0,
		TextSnippet: "some text", Note: "my note",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("bookmark id is 0")
	}

	bms, _ := store.ListBookmarks(1)
	if len(bms) != 1 {
		t.Fatalf("got %d bookmarks, want 1", len(bms))
	}
	if bms[0].Note != "my note" {
		t.Errorf("note = %q", bms[0].Note)
	}

	store.DeleteBookmark(id)
	bms, _ = store.ListBookmarks(1)
	if len(bms) != 0 {
		t.Errorf("after delete: got %d bookmarks", len(bms))
	}
}

func TestSettings(t *testing.T) {
	store := testStore(t)

	store.SetSetting("key1", "value1")
	store.SetSetting("key2", "value2")

	v, _ := store.GetSetting("key1")
	if v != "value1" {
		t.Errorf("got %q, want value1", v)
	}

	all, _ := store.GetAllSettings()
	if len(all) != 2 {
		t.Errorf("got %d settings, want 2", len(all))
	}

	// Update
	store.SetSetting("key1", "updated")
	v, _ = store.GetSetting("key1")
	if v != "updated" {
		t.Errorf("got %q, want updated", v)
	}
}

func TestChunks(t *testing.T) {
	store := testStore(t)

	store.InsertChunk(Chunk{BookID: 1, ChapterIdx: 0, ChunkIdx: 0, Content: "The monster appeared", StartWord: 0, EndWord: 3})
	store.InsertChunk(Chunk{BookID: 1, ChapterIdx: 0, ChunkIdx: 1, Content: "Victor fled in terror", StartWord: 3, EndWord: 7})
	store.InsertChunk(Chunk{BookID: 1, ChapterIdx: 1, ChunkIdx: 0, Content: "The creature spoke", StartWord: 0, EndWord: 3})

	count, _ := store.ChunkCount(1)
	if count != 3 {
		t.Errorf("chunk count: got %d, want 3", count)
	}

	results, _ := store.SearchChunks(1, "monster")
	if len(results) != 1 {
		t.Errorf("search 'monster': got %d, want 1", len(results))
	}

	results, _ = store.SearchChunks(1, "nonexistent")
	if len(results) != 0 {
		t.Errorf("search 'nonexistent': got %d, want 0", len(results))
	}
}

func TestGetBookAndNil(t *testing.T) {
	store := testStore(t)

	// Nonexistent book should return nil, not error
	book, err := store.GetBook(999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if book != nil {
		t.Error("expected nil for nonexistent book")
	}

	// Same for position
	pos, err := store.GetPosition(999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos != nil {
		t.Error("expected nil for nonexistent position")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
