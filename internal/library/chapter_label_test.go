package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestIsChapterLabel(t *testing.T) {
	labels := []string{
		"Chapter 7", "chapter1", "Chapter 3", "chapter 5", "Track 1",
		"track 12", "Part 3", "Part III", "Disc 2", "CD 1", "Side", "01", "7", "chapter",
	}
	for _, s := range labels {
		if !isChapterLabel(s) {
			t.Errorf("isChapterLabel(%q) = false, want true", s)
		}
	}
	// Real authors/titles must NOT be flagged.
	notLabels := []string{
		"Joanne Harris", "Erich Maria Remarque", "Partridge", "Sectional",
		"Charles Dickens", "Track Palin", "Chapter House", "Sidney Sheldon", "",
	}
	for _, s := range notLabels {
		if isChapterLabel(s) {
			t.Errorf("isChapterLabel(%q) = true, want false", s)
		}
	}
}

// A work whose author is a chapter label gets its title+author re-derived from
// its publisher EPUB (the Chocolat #53 case).
func TestHealChapterLabelAuthors_FromEPUB(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, err := store.CreateWork("chocolate", "Chapter 7")
	if err != nil {
		t.Fatal(err)
	}
	// The clean EPUB living in the same work.
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: "/lib/chocolat.epub", Filename: "chocolat.epub",
		Format: "epub", MediaType: "text", Origin: "publisher_epub",
		Title: "Chocolat", Author: "Joanne Harris",
	}); err != nil {
		t.Fatal(err)
	}
	books, _ := store.ListBooks()
	var ids []int64
	for _, b := range books {
		ids = append(ids, b.ID)
	}
	if err := store.AssignBooksToWork(workID, ids); err != nil {
		t.Fatal(err)
	}

	fixed, err := HealChapterLabelAuthors(store)
	if err != nil {
		t.Fatal(err)
	}
	if fixed != 1 {
		t.Fatalf("fixed = %d, want 1", fixed)
	}
	w, _ := store.GetWork(workID)
	if w.Author != "Joanne Harris" {
		t.Errorf("author = %q, want %q", w.Author, "Joanne Harris")
	}
	if w.Title != "Chocolat" {
		t.Errorf("title = %q, want %q (adopted from EPUB)", w.Title, "Chocolat")
	}

	// Idempotent: a second pass changes nothing.
	if fixed2, _ := HealChapterLabelAuthors(store); fixed2 != 0 {
		t.Errorf("second pass fixed = %d, want 0 (idempotent)", fixed2)
	}
}

// With no clean text source, a chapter-label author is blanked (better than
// "Chapter 7") and the title is left as-is.
func TestHealChapterLabelAuthors_BlanksWhenNoSource(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, err := store.CreateWork("Some Audiobook", "Track 1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: "/lib/a.mp3", Filename: "a.mp3", Format: "mp3",
		MediaType: "audio", Origin: "narrator_recording", Author: "Track 1",
	}); err != nil {
		t.Fatal(err)
	}
	books, _ := store.ListBooks()
	var ids []int64
	for _, b := range books {
		ids = append(ids, b.ID)
	}
	store.AssignBooksToWork(workID, ids)

	if _, err := HealChapterLabelAuthors(store); err != nil {
		t.Fatal(err)
	}
	w, _ := store.GetWork(workID)
	if w.Author != "" {
		t.Errorf("author = %q, want blank", w.Author)
	}
	if w.Title != "Some Audiobook" {
		t.Errorf("title = %q, want unchanged", w.Title)
	}
}

// A work with a real author is never touched.
func TestHealChapterLabelAuthors_LeavesGoodAuthors(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	workID, _ := store.CreateWork("1984", "George Orwell")
	fixed, err := HealChapterLabelAuthors(store)
	if err != nil {
		t.Fatal(err)
	}
	if fixed != 0 {
		t.Errorf("fixed = %d, want 0", fixed)
	}
	w, _ := store.GetWork(workID)
	if w.Author != "George Orwell" {
		t.Errorf("author = %q, want unchanged", w.Author)
	}
}
