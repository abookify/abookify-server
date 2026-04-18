package library

import (
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// buildWords returns a word list where each token is one word long, starting
// at second `startAt` and advancing 1s per word. Handy for exercising slicing.
func buildWords(texts []string, startAt float64) []db.SyncTimestamp {
	words := make([]db.SyncTimestamp, len(texts))
	for i, w := range texts {
		words[i] = db.SyncTimestamp{
			Start: startAt + float64(i),
			End:   startAt + float64(i) + 0.5,
			Word:  w,
		}
	}
	return words
}

func TestSplitTranscript_SplitsCleanly(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Book to hold the transcript.
	store.UpsertBook(db.Book{Path: "/x/t.txt", Filename: "t.txt", Format: "transcript", MediaType: "text"})
	books, _ := store.ListBooks()
	bookID := books[0].ID

	// Seed a flat single-chapter transcript to prove the splitter replaces it.
	store.InsertChapter(db.Chapter{BookID: bookID, Index: 0, Title: "Whole", Content: "ignore me"})

	// 20 words, first 10 belong to Chapter 1 (0-10s), last 10 to Chapter 2 (10-20s).
	texts := strings.Fields("alpha bravo charlie delta echo foxtrot golf hotel india juliet " +
		"kilo lima mike november oscar papa quebec romeo sierra tango")
	words := buildWords(texts, 0)
	detected := []DetectedChapter{
		{Index: 0, Number: 1, Kind: "chapter", Title: "Chapter 1", StartSec: 0, EndSec: 10, Confidence: 1.0},
		{Index: 1, Number: 2, Kind: "chapter", Title: "Chapter 2", StartSec: 10, EndSec: 20, Confidence: 1.0},
	}

	n, err := SplitTranscriptByChapters(store, bookID, words, detected)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 chapters written, got %d", n)
	}

	chs, _ := store.ListChapters(bookID)
	if len(chs) != 2 {
		t.Fatalf("listed %d chapters, want 2", len(chs))
	}
	c1, _ := store.GetChapterContent(bookID, 0)
	c2, _ := store.GetChapterContent(bookID, 1)
	if c1.WordCount != 10 || c2.WordCount != 10 {
		t.Errorf("word counts: c1=%d c2=%d, want 10 each", c1.WordCount, c2.WordCount)
	}
	if !strings.Contains(c1.Content, "alpha") || strings.Contains(c1.Content, "kilo") {
		t.Errorf("c1 content wrong: %q", c1.Content)
	}
	if !strings.Contains(c2.Content, "kilo") || strings.Contains(c2.Content, "alpha") {
		t.Errorf("c2 content wrong: %q", c2.Content)
	}
	if c1.StartSec != 0 || c1.EndSec != 10 {
		t.Errorf("c1 time range: %v..%v", c1.StartSec, c1.EndSec)
	}
}

func TestSplitTranscript_LastChapterAbsorbsTrailingWords(t *testing.T) {
	// If Whisper produces words past the last chapter's EndSec (e.g. outro),
	// they should roll into the last chapter rather than vanish.
	store, cleanup := newTestStore(t)
	defer cleanup()

	store.UpsertBook(db.Book{Path: "/x/t2.txt", Filename: "t2.txt", Format: "transcript", MediaType: "text"})
	books, _ := store.ListBooks()
	bookID := books[0].ID

	words := buildWords(strings.Fields("one two three four five six seven eight"), 0)
	// Detected says chapter 2 ends at 4s, but we have words up to ~7s.
	detected := []DetectedChapter{
		{Index: 0, Title: "Chapter 1", StartSec: 0, EndSec: 4},
		{Index: 1, Title: "Chapter 2", StartSec: 4, EndSec: 4}, // 0-length EndSec
	}
	if _, err := SplitTranscriptByChapters(store, bookID, words, detected); err != nil {
		t.Fatal(err)
	}
	c2, _ := store.GetChapterContent(bookID, 1)
	if c2.WordCount != 4 {
		t.Errorf("trailing words should land in last chapter; got wc=%d content=%q", c2.WordCount, c2.Content)
	}
}

func TestSplitTranscript_Empty(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	n, err := SplitTranscriptByChapters(store, 1, nil, nil)
	if err != nil || n != 0 {
		t.Errorf("empty input: n=%d err=%v", n, err)
	}
}
