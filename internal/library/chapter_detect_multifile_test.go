package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestLocateChapterInFiles_FirstFile(t *testing.T) {
	slices := []fileSlice{
		{book: db.Book{ID: 1, Duration: 600}, baseOffset: 0,
			words: []db.SyncTimestamp{{Start: 0, End: 600}}},
		{book: db.Book{ID: 2, Duration: 600}, baseOffset: 600,
			words: []db.SyncTimestamp{{Start: 600, End: 1200}}},
	}
	idx, lStart, lEnd := locateChapterInFiles(slices, 150, 400)
	if idx != 0 {
		t.Errorf("chapter at t=150: want file 0, got %d", idx)
	}
	if lStart != 150 || lEnd != 400 {
		t.Errorf("locals: [%v, %v), want [150, 400)", lStart, lEnd)
	}
}

func TestLocateChapterInFiles_SecondFile(t *testing.T) {
	slices := []fileSlice{
		{book: db.Book{ID: 1, Duration: 600}, baseOffset: 0,
			words: []db.SyncTimestamp{{Start: 0, End: 600}}},
		{book: db.Book{ID: 2, Duration: 600}, baseOffset: 600,
			words: []db.SyncTimestamp{{Start: 600, End: 1200}}},
	}
	idx, lStart, _ := locateChapterInFiles(slices, 800, 1100)
	if idx != 1 {
		t.Errorf("chapter at t=800: want file 1, got %d", idx)
	}
	if lStart != 200 {
		t.Errorf("local start: %v, want 200 (800 - 600 offset)", lStart)
	}
}

func TestLocateChapterInFiles_SpansTwoFiles(t *testing.T) {
	slices := []fileSlice{
		{book: db.Book{ID: 1, Duration: 600}, baseOffset: 0,
			words: []db.SyncTimestamp{{Start: 0, End: 600}}},
		{book: db.Book{ID: 2, Duration: 600}, baseOffset: 600,
			words: []db.SyncTimestamp{{Start: 600, End: 1200}}},
	}
	// Chapter starts in file 0 at t=500, ends in file 1 at t=800.
	idx, lStart, lEnd := locateChapterInFiles(slices, 500, 800)
	if idx != 0 {
		t.Errorf("span: want file 0 (where it starts), got %d", idx)
	}
	if lStart != 500 {
		t.Errorf("local start: %v, want 500", lStart)
	}
	// localEnd should be clamped to this file's duration.
	if lEnd > 600 {
		t.Errorf("local end should be clamped to file boundary; got %v", lEnd)
	}
}

func TestLocateChapterInFiles_BeyondLast(t *testing.T) {
	slices := []fileSlice{
		{book: db.Book{ID: 1, Duration: 600}, baseOffset: 0,
			words: []db.SyncTimestamp{{Start: 0, End: 600}}},
	}
	idx, _, _ := locateChapterInFiles(slices, 1000, 1200)
	if idx != -1 {
		t.Errorf("chapter past last file should return -1, got %d", idx)
	}
}
