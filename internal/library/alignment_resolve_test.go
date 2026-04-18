package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestResolveAlignmentPath_Direct(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, _ := store.CreateWork("Test", "Author")
	store.UpsertBook(db.Book{WorkID: workID, Path: "/a/audio.mp3", Filename: "audio.mp3", Format: "mp3", MediaType: "audio"})
	store.UpsertBook(db.Book{WorkID: workID, Path: "/a/book.epub", Filename: "book.epub", Format: "epub", MediaType: "text"})
	books, _ := store.ListBooks()
	audioID, epubID := books[0].ID, books[1].ID

	store.SaveAlignment(db.Alignment{
		WorkID: workID, FromBookID: audioID, ToBookID: epubID,
		Unit: "word", Method: "edit-distance", Confidence: 0.95, Pairs: "[]",
	})

	path := ResolveAlignmentPath(store, workID, audioID, epubID, "word")
	if len(path) != 1 {
		t.Fatalf("want 1-step direct path, got %d", len(path))
	}
	if path[0].Reversed {
		t.Error("direct path should not be reversed")
	}
}

func TestResolveAlignmentPath_DirectReversed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, _ := store.CreateWork("Test", "Author")
	store.UpsertBook(db.Book{WorkID: workID, Path: "/b/audio.mp3", Filename: "audio.mp3", Format: "mp3", MediaType: "audio"})
	store.UpsertBook(db.Book{WorkID: workID, Path: "/b/book.epub", Filename: "book.epub", Format: "epub", MediaType: "text"})
	books, _ := store.ListBooks()
	audioID, epubID := books[0].ID, books[1].ID

	// Store alignment from epub → audio (reversed relative to our query)
	store.SaveAlignment(db.Alignment{
		WorkID: workID, FromBookID: epubID, ToBookID: audioID,
		Unit: "word", Method: "test", Confidence: 0.9, Pairs: "[]",
	})

	// Query audio → epub — should find the reversed alignment.
	path := ResolveAlignmentPath(store, workID, audioID, epubID, "word")
	if len(path) != 1 {
		t.Fatalf("want 1-step path, got %d", len(path))
	}
	if !path[0].Reversed {
		t.Error("should be reversed since we stored epub→audio")
	}
}

func TestResolveAlignmentPath_Transitive(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, _ := store.CreateWork("Test", "Author")
	store.UpsertBook(db.Book{WorkID: workID, Path: "/c/audio.mp3", Filename: "audio.mp3", Format: "mp3", MediaType: "audio"})
	store.UpsertBook(db.Book{WorkID: workID, Path: "/c/transcript.json", Filename: "transcript.json", Format: "transcript", MediaType: "text"})
	store.UpsertBook(db.Book{WorkID: workID, Path: "/c/book.epub", Filename: "book.epub", Format: "epub", MediaType: "text"})
	books, _ := store.ListBooks()
	audioID, transcriptID, epubID := books[0].ID, books[1].ID, books[2].ID

	// audio → transcript (whisper-native)
	store.SaveAlignment(db.Alignment{
		WorkID: workID, FromBookID: audioID, ToBookID: transcriptID,
		Unit: "word", Method: "whisper-native", Confidence: 1.0, Pairs: "[]",
	})
	// transcript → epub (edit-distance)
	store.SaveAlignment(db.Alignment{
		WorkID: workID, FromBookID: transcriptID, ToBookID: epubID,
		Unit: "word", Method: "edit-distance", Confidence: 0.92, Pairs: "[]",
	})

	// Query audio → epub — should find 2-step transitive path.
	path := ResolveAlignmentPath(store, workID, audioID, epubID, "word")
	if len(path) != 2 {
		t.Fatalf("want 2-step transitive path, got %d", len(path))
	}
	if path[0].Alignment.Method != "whisper-native" {
		t.Errorf("step 1 should be whisper-native, got %s", path[0].Alignment.Method)
	}
	if path[1].Alignment.Method != "edit-distance" {
		t.Errorf("step 2 should be edit-distance, got %s", path[1].Alignment.Method)
	}
}

func TestResolveAlignmentPath_NoPath(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, _ := store.CreateWork("Test", "Author")
	store.UpsertBook(db.Book{WorkID: workID, Path: "/d/audio.mp3", Filename: "audio.mp3", Format: "mp3", MediaType: "audio"})
	store.UpsertBook(db.Book{WorkID: workID, Path: "/d/book.epub", Filename: "book.epub", Format: "epub", MediaType: "text"})
	books, _ := store.ListBooks()
	audioID, epubID := books[0].ID, books[1].ID

	// No alignments stored at all.
	path := ResolveAlignmentPath(store, workID, audioID, epubID, "word")
	if path != nil {
		t.Errorf("expected nil path, got %d steps", len(path))
	}
}

func TestResolveAlignmentPath_SameBook(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	path := ResolveAlignmentPath(store, 1, 42, 42, "word")
	if path != nil {
		t.Errorf("same book should return nil path")
	}
}
