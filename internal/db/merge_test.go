package db

import "testing"

// Merging two works that EACH have chapter_links 0..N must not crash on the
// chapter_links unique index — and post-merge the target should hold BOTH audio
// editions with all their links preserved (LibriVox + AI TTS narrations).
func TestMergeWorksChapterLinksNoCollision(t *testing.T) {
	store := testStore(t)

	w1, err := store.CreateWork("Call of the Wild", "Jack London")
	if err != nil {
		t.Fatal(err)
	}
	w2, err := store.CreateWork("call-of-the-wild-ai", "Jack London")
	if err != nil {
		t.Fatal(err)
	}
	// w1: LibriVox audio + a text book. w2: the AI TTS audio.
	must := func(e error) {
		if e != nil {
			t.Fatal(e)
		}
	}
	must(store.UpsertBook(Book{WorkID: w1, Path: "/librivox.mp3", Filename: "librivox.mp3", Format: "mp3", MediaType: "audio"}))
	must(store.UpsertBook(Book{WorkID: w1, Path: "/cotw.epub", Filename: "cotw.epub", Format: "epub", MediaType: "text"}))
	must(store.UpsertBook(Book{WorkID: w2, Path: "/ai.mp3", Filename: "ai.mp3", Format: "mp3", MediaType: "audio"}))

	byPath := map[string]int64{}
	for _, b := range mustListBooks(t, store) {
		byPath[b.Path] = b.ID
	}
	a1, tx, a2 := byPath["/librivox.mp3"], byPath["/cotw.epub"], byPath["/ai.mp3"]

	// Overlapping audio_index (0,1,2) on both works — the collision case.
	for i := 0; i < 3; i++ {
		must(store.InsertChapterLink(w1, ChapterLink{AudioBookID: a1, AudioIndex: i, TextBookID: tx, TextIndex: i}))
		must(store.InsertChapterLink(w2, ChapterLink{AudioBookID: a2, AudioIndex: i, TextBookID: tx, TextIndex: i}))
	}

	if err := store.MergeWorks(w1, w2); err != nil {
		t.Fatalf("merge crashed (the bug): %v", err)
	}

	work, err := store.GetWork(w1)
	if err != nil || work == nil {
		t.Fatalf("get merged work: %v", err)
	}
	if len(work.AudioFiles) != 2 {
		t.Errorf("post-merge audio editions = %d, want 2 (both narrations)", len(work.AudioFiles))
	}
	links, _ := store.GetChapterLinks(w1)
	if len(links) != 6 {
		t.Errorf("chapter_links after merge = %d, want 6 (3 per edition, no clobber)", len(links))
	}
	if w, _ := store.GetWork(w2); w != nil {
		t.Error("source work should be deleted after merge")
	}
}
