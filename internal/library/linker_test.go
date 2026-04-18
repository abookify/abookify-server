package library

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// newTestStore spins up an empty in-temp-dir sqlite store and returns a cleanup.
func newTestStore(t *testing.T) (*db.Store, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "linker-test-*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("open db: %v", err)
	}
	return store, func() {
		store.Close()
		os.RemoveAll(dir)
	}
}

// seedWork inserts a work with a single audio book and text book with N chapters.
func seedWork(t *testing.T, store *db.Store, numTextChapters int, detectedAudioChapters int) *db.Work {
	t.Helper()
	workID, err := store.CreateWork("Test Book", "Test Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	audioBook := db.Book{
		WorkID: workID, Path: "/x/audio.mp3", Filename: "audio.mp3",
		Format: "mp3", MediaType: "audio", Title: "Test Book",
	}
	if err := store.UpsertBook(audioBook); err != nil {
		t.Fatalf("upsert audio: %v", err)
	}
	textBook := db.Book{
		WorkID: workID, Path: "/x/book.epub", Filename: "book.epub",
		Format: "epub", MediaType: "text", Title: "Test Book",
	}
	if err := store.UpsertBook(textBook); err != nil {
		t.Fatalf("upsert text: %v", err)
	}
	// Read back IDs.
	books, _ := store.ListBooks()
	var audioID, textID int64
	for _, b := range books {
		switch b.Path {
		case "/x/audio.mp3":
			audioID = b.ID
		case "/x/book.epub":
			textID = b.ID
		}
	}
	// Text chapters "Chapter 1" .. "Chapter N".
	for i := 0; i < numTextChapters; i++ {
		store.InsertChapter(db.Chapter{
			BookID: textID, Index: i, Title: titleFor("chapter", i+1), Content: "content",
		})
	}
	// ChapterCount is computed on read by GetWork — no explicit setter needed.
	// Detected audio chapters.
	for i := 0; i < detectedAudioChapters; i++ {
		store.InsertChapter(db.Chapter{
			BookID: audioID, Index: i, Title: titleFor("chapter", i+1),
			Src: "detected", StartSec: float64(i) * 100, EndSec: float64(i+1) * 100, Confidence: 1.0,
		})
	}
	// Re-load the work so it has the fresh chapter counts.
	fresh, err := store.GetWork(workID)
	if err != nil || fresh == nil {
		t.Fatalf("get work: %v", err)
	}
	return fresh
}

func TestLinkChapters_DetectedAudioToEbook(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	work := seedWork(t, store, 5, 5) // 5 detected chapters, 5 ebook chapters
	if err := LinkChapters(store, work); err != nil {
		t.Fatalf("link: %v", err)
	}
	links, _ := store.GetChapterLinks(work.ID)
	if len(links) != 5 {
		t.Fatalf("want 5 links, got %d", len(links))
	}
	// Each audio_index N should map to text_index N.
	for _, l := range links {
		if l.TextIndex != l.AudioIndex {
			t.Errorf("link mismatch: audio_index=%d text_index=%d", l.AudioIndex, l.TextIndex)
		}
		if l.Confidence < 0.9 {
			t.Errorf("expected high confidence, got %v", l.Confidence)
		}
	}
}

func TestLinkChapters_FallbackToFileList(t *testing.T) {
	// When the single audio book has NO detected chapters, we fall back to the
	// file-list behavior. With one audio file titled "Test Book" and ebook
	// chapters named "Chapter 1..5", no match is possible → 0 links.
	store, cleanup := newTestStore(t)
	defer cleanup()

	work := seedWork(t, store, 5, 0) // no detected chapters
	if err := LinkChapters(store, work); err != nil {
		t.Fatalf("link: %v", err)
	}
	links, _ := store.GetChapterLinks(work.ID)
	if len(links) != 0 {
		t.Errorf("unmatched fallback should produce 0 links, got %d", len(links))
	}
}

func TestLinkChapters_ReRunReplacesStaleLinks(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	work := seedWork(t, store, 10, 10)
	if err := LinkChapters(store, work); err != nil {
		t.Fatalf("link: %v", err)
	}
	if links, _ := store.GetChapterLinks(work.ID); len(links) != 10 {
		t.Fatalf("initial: want 10 links, got %d", len(links))
	}

	// Re-detect with fewer chapters (simulating a re-transcription with different results).
	audioBookID := work.AudioFiles[0].ID
	store.DeleteChaptersByBook(audioBookID)
	for i := 0; i < 5; i++ {
		store.InsertChapter(db.Chapter{
			BookID: audioBookID, Index: i, Title: titleFor("chapter", i+1),
			Src: "detected", StartSec: float64(i) * 100, EndSec: float64(i+1) * 100,
		})
	}
	fresh, _ := store.GetWork(work.ID)
	if err := LinkChapters(store, fresh); err != nil {
		t.Fatalf("relink: %v", err)
	}
	links, _ := store.GetChapterLinks(work.ID)
	if len(links) != 5 {
		t.Errorf("after re-detect want 5 links, got %d (stale entries not cleaned up)", len(links))
	}
}

func TestLinkChapters_NoTextBook(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, _ := store.CreateWork("Audio Only", "")
	store.UpsertBook(db.Book{
		WorkID: workID, Path: "/y/audio.mp3", Filename: "audio.mp3",
		Format: "mp3", MediaType: "audio",
	})
	fresh, _ := store.GetWork(workID)
	if err := LinkChapters(store, fresh); err != nil {
		t.Fatalf("link: %v", err)
	}
	// Should no-op without error.
}
