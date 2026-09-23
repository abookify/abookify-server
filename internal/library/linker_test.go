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

// Carol's stave01 file opens with a 17-second Preface and then all of Stave
// One; the file links to the chapter that covers it, not the one at its
// first second (card 37, 2026-09-22).
func TestDominantChapter_PrefersCoverageOverFirstSecond(t *testing.T) {
	timeline := []chapterStartAt{{32.8, 0}, {49.2, 1}, {2318, 2}, {4431, 3}}
	if got := dominantChapter(timeline, 0, 2315); got != 1 {
		t.Errorf("stave01 (0–2315 s) should link to Stave One (idx 1), got %d", got)
	}
	if got := dominantChapter(timeline, 2318, 4431); got != 2 {
		t.Errorf("stave02 should link to idx 2, got %d", got)
	}
	// A file that ends before every chapter starts links to the first.
	if got := dominantChapter(timeline, 0, 20); got != 0 {
		t.Errorf("pre-chapter window should link to the first chapter, got %d", got)
	}
}

// A payload computed against a chapter list that has since shifted by one
// (a title page dropped after the alignment) must not drive titles or links;
// a payload that merely skips a boilerplate chapter still matches.
func TestAlignmentMatchesChapters_StaleIsRefused(t *testing.T) {
	var chs []db.Chapter
	for i, w := range []int{1296, 1441, 4607, 3458, 11086, 8710} {
		chs = append(chs, db.Chapter{Index: i, WordCount: w})
	}
	fresh := &AnchorAlignmentPayload{}
	for i, w := range []int{1300, 1450, 4640, 3480, 11150, 8760} { // aligner tokens run ~1 % high
		fresh.EbookChapters = append(fresh.EbookChapters, ChapterSpan{Index: i, Len: w})
	}
	if !AlignmentMatchesChapters(fresh, chs) {
		t.Fatal("a fresh payload within tokenizer tolerance must match")
	}
	skipping := &AnchorAlignmentPayload{EbookChapters: fresh.EbookChapters[2:]} // boilerplate chapters not aligned
	if !AlignmentMatchesChapters(skipping, chs) {
		t.Fatal("a payload that skips leading chapters must still match")
	}
	shifted := &AnchorAlignmentPayload{}
	for i, w := range []int{231, 1300, 1450, 4640, 3480, 11150, 8760} { // computed when a 231-word title page led the book
		shifted.EbookChapters = append(shifted.EbookChapters, ChapterSpan{Index: i, Len: w})
	}
	if AlignmentMatchesChapters(shifted, chs) {
		t.Fatal("a one-chapter shift must read as stale")
	}
}
