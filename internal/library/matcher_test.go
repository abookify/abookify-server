package library

import (
	"fmt"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// Regression test: 8 mp3 files from one directory, scanned in two passes
// (e.g. first 4 then second 4, as would happen on a rescan after partial
// failure or a second batch of files landing), must all land in the SAME
// work. Before the FindWorkByAudioDir guard, each pass created a fresh
// duplicate work for the directory — Norm Macdonald "Based on a True Story"
// wound up as 7 near-identical works in production.
func TestMatchAndCreateWorks_DoesNotDuplicateOnRescan(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	dir := "/library/audiobooks/Norm Macdonald – Based on a True Story"

	// First pass: drop in files 01–04.
	for _, fn := range []string{"01.mp3", "02.mp3", "03.mp3", "04.mp3"} {
		if err := store.UpsertBook(db.Book{
			Path: dir + "/" + fn, Filename: fn, Format: "mp3",
			MediaType: "audio", Title: "Based on a True Story (Unabridged)",
			Author: "Norm Macdonald", Album: "Based on a True Story (Unabridged)",
		}); err != nil {
			t.Fatalf("upsert %s: %v", fn, err)
		}
	}
	if err := MatchAndCreateWorks(store); err != nil {
		t.Fatalf("first match: %v", err)
	}
	works, _ := store.ListWorks()
	if got := len(works); got != 1 {
		t.Fatalf("after first pass: want 1 work, got %d", got)
	}
	firstID := works[0].ID

	// Second pass: drop in files 05–08 as fresh unassigned books.
	for _, fn := range []string{"05.mp3", "06.mp3", "07.mp3", "08.mp3"} {
		if err := store.UpsertBook(db.Book{
			Path: dir + "/" + fn, Filename: fn, Format: "mp3",
			MediaType: "audio", Title: "Based on a True Story (Unabridged)",
			Author: "Norm Macdonald", Album: "Based on a True Story (Unabridged)",
		}); err != nil {
			t.Fatalf("upsert %s: %v", fn, err)
		}
	}
	if err := MatchAndCreateWorks(store); err != nil {
		t.Fatalf("second match: %v", err)
	}

	works, _ = store.ListWorks()
	if got := len(works); got != 1 {
		t.Fatalf("after second pass: want 1 work (reuse), got %d duplicates", got)
	}
	if works[0].ID != firstID {
		t.Fatalf("work id changed across passes: %d → %d", firstID, works[0].ID)
	}

	// All 8 files should now live on the single work.
	w, err := store.GetWork(firstID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if got := len(w.AudioFiles); got != 8 {
		t.Fatalf("want 8 audio files on work, got %d", got)
	}
}

// Regression: untagged mp3s with filename-only Title (01.mp3, 02.mp3, …) and
// no Album or Author tags should produce a work titled from the parent dir,
// not a work titled "01". Observed with A Clockwork Orange.
func TestMatchAndCreateWorks_UntaggedMP3sUseDirName(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	dir := "/library/audiobooks/Anthony Burgess – A Clockwork Orange"
	for i := 1; i <= 6; i++ {
		fn := "0" + string(rune('0'+i)) + ".mp3"
		if err := store.UpsertBook(db.Book{
			Path: dir + "/" + fn, Filename: fn, Format: "mp3",
			MediaType: "audio",
			Title:     "0" + string(rune('0'+i)), // filename-derived
		}); err != nil {
			t.Fatalf("upsert %s: %v", fn, err)
		}
	}
	if err := MatchAndCreateWorks(store); err != nil {
		t.Fatalf("match: %v", err)
	}
	works, _ := store.ListWorks()
	if len(works) != 1 {
		t.Fatalf("want 1 work, got %d", len(works))
	}
	if works[0].Title == "01" || looksLikeTrackNumber(works[0].Title) {
		t.Fatalf("title fell through to track number: %q", works[0].Title)
	}
	// Full dir name is used as title when no Album tag — splitting
	// "Author – Title" is unreliable (some dirs are "Title - Author").
	if works[0].Title != "Anthony Burgess – A Clockwork Orange" {
		t.Fatalf("want full dir name as title, got %q", works[0].Title)
	}
}

// Regression: a work that already exists in the DB with a bogus title
// (a track-number string like "01" carried over from a pre-looksLikeTrackNumber
// matcher version) gets self-healed when a fresh book in the same dir
// triggers a re-match. The matcher finds the legacy work via FindWorkByAudioDir
// and rewrites its title using the dir name. Observed in production: work #10
// "01" / Clockwork Orange had the bogus title stuck across many rescans.
func TestMatchAndCreateWorks_SelfHealsLegacyTrackNumberTitle(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	dir := "/library/audiobooks/Anthony Burgess – A Clockwork Orange"

	// Pre-create the work with the bogus title that legacy code produced,
	// owning 5 of 6 audio files (already assigned).
	legacyID, err := store.CreateWork("01", "")
	if err != nil {
		t.Fatalf("create legacy work: %v", err)
	}
	for i := 1; i <= 5; i++ {
		fn := fmt.Sprintf("0%d.mp3", i)
		if err := store.UpsertBook(db.Book{
			Path: dir + "/" + fn, Filename: fn, Format: "mp3",
			MediaType: "audio",
			Title:     fmt.Sprintf("0%d", i),
		}); err != nil {
			t.Fatalf("upsert %s: %v", fn, err)
		}
	}
	owned, _ := store.ListBooks()
	var ownedIDs []int64
	for _, b := range owned {
		ownedIDs = append(ownedIDs, b.ID)
	}
	if err := store.AssignBooksToWork(legacyID, ownedIDs); err != nil {
		t.Fatalf("assign owned: %v", err)
	}

	// A 6th file lands in the same dir, unassigned — modeling a fresh
	// rescan / new file event. The matcher must group it with the existing
	// work's directory and self-heal the bogus title.
	if err := store.UpsertBook(db.Book{
		Path: dir + "/06.mp3", Filename: "06.mp3", Format: "mp3",
		MediaType: "audio", Title: "06",
	}); err != nil {
		t.Fatalf("upsert new file: %v", err)
	}

	if err := MatchAndCreateWorks(store); err != nil {
		t.Fatalf("match: %v", err)
	}

	w, err := store.GetWork(legacyID)
	if err != nil || w == nil {
		t.Fatalf("get healed work: %v", err)
	}
	if w.Title == "01" || looksLikeTrackNumber(w.Title) {
		t.Fatalf("title still bogus after heal: %q", w.Title)
	}
	if w.Title != "Anthony Burgess – A Clockwork Orange" {
		t.Fatalf("want healed title from dir, got %q", w.Title)
	}
}

// Regression: untagged multi-file audiobooks with descriptive filenames like
// "why-we-sleep-part1.mp3" should still prefer the directory name, since no
// Album tag means the Title is filename-derived. The dir name is typically
// more authoritative (includes author, no "part1" suffix).
func TestMatchAndCreateWorks_FilenameVsDirFallback(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	dir := "/library/audiobooks/Why We Sleep - Matthew Walker"
	for i := 1; i <= 6; i++ {
		fn := fmt.Sprintf("why-we-sleep-part%d.mp3", i)
		if err := store.UpsertBook(db.Book{
			Path: dir + "/" + fn, Filename: fn, Format: "mp3",
			MediaType: "audio",
			Title:     fmt.Sprintf("why we sleep part%d", i),
		}); err != nil {
			t.Fatalf("upsert %s: %v", fn, err)
		}
	}
	if err := MatchAndCreateWorks(store); err != nil {
		t.Fatalf("match: %v", err)
	}
	works, _ := store.ListWorks()
	if len(works) != 1 {
		t.Fatalf("want 1 work, got %d", len(works))
	}
	if works[0].Title != "Why We Sleep - Matthew Walker" {
		t.Fatalf("want full dir name as title, got %q", works[0].Title)
	}
}
