package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

// The canonical answer must DECLARE incoherence rather than paper it: six
// same-voice files split across two edition labels (PJ's two-editions-of-
// three) is a data problem no renderer can present consistently, and the
// cross-surface assert must go red on it.
func TestWorkCanonFlagsEditionLabelSplit(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("Split", "")
	for i, ed := range []string{"Kokoro · Fable", "Kokoro · Fable", "Kokoro · Fable", "", "", ""} {
		store.UpsertBook(db.Book{WorkID: wid,
			Path:     "/generated/tts-book-9/chapter-00" + string(rune('0'+i)) + ".mp3",
			Filename: "chapter-00" + string(rune('0'+i)) + ".mp3",
			Format:   "mp3", MediaType: "audio", Origin: "tts_kokoro",
			Album: "bm_fable", Edition: ed, Duration: 100})
	}
	store.UpsertBook(db.Book{WorkID: wid, Path: "/library/ebooks/x.epub",
		Filename: "x.epub", Format: "epub", MediaType: "text", Origin: "publisher_epub"})

	c, err := BuildWorkCanon(store, wid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Coherent {
		t.Errorf("label-split work reported coherent — the assert could never go red on PJ's actual state")
	}
	if len(c.Editions) != 1 {
		t.Errorf("editions = %d, want 1 (one directory, one voice — the split is a LABEL defect, not two narrations)", len(c.Editions))
	}
	if c.TotalAudioFiles != 6 || c.Active.AudioFiles != 6 {
		t.Errorf("totals: total=%d active=%d, want 6/6", c.TotalAudioFiles, c.Active.AudioFiles)
	}
}

// A clean single-edition work is coherent, and Active carries the header
// numbers both surfaces must show.
func TestWorkCanonCleanWork(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("Clean", "")
	for i := 0; i < 3; i++ {
		store.UpsertBook(db.Book{WorkID: wid,
			Path:     "/generated/tts-book-1/chapter-00" + string(rune('0'+i)) + ".mp3",
			Filename: "chapter-00" + string(rune('0'+i)) + ".mp3",
			Format:   "mp3", MediaType: "audio", Origin: "tts_kokoro",
			Album: "bm_fable", Edition: "Kokoro · Fable", Duration: 100})
	}
	store.UpsertBook(db.Book{WorkID: wid, Path: "/library/ebooks/c.epub",
		Filename: "c.epub", Format: "epub", MediaType: "text", Origin: "publisher_epub"})
	books, _ := store.ListBooks()
	for _, b := range books {
		if b.Format == "epub" {
			store.InsertChapter(db.Chapter{BookID: b.ID, Index: 0, Title: "One", Content: "hello world", WordCount: 2})
		}
	}
	c, err := BuildWorkCanon(store, wid)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Coherent {
		t.Errorf("clean work incoherent: %v", c.Problems)
	}
	if c.Active.AudioFiles != 3 || c.Active.TextChapters != 1 || c.TotalTexts != 1 {
		t.Errorf("active audio=%d textCh=%d texts=%d, want 3/1/1", c.Active.AudioFiles, c.Active.TextChapters, c.TotalTexts)
	}
}

// When a human recording and one of ours coexist, the HUMAN one is the
// canonical default (META 2026-08-09, PJ's philosophy: their property beats
// our product on the most visible surface). Ours wins only when it is the
// only narration.
func TestWorkCanonHumanNarrationDefaults(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("Both", "")
	store.UpsertBook(db.Book{WorkID: wid, Path: "/library/audiobooks/human/01.mp3",
		Filename: "01.mp3", Format: "mp3", MediaType: "audio", Origin: "narrator_recording", Duration: 100})
	for i := 0; i < 3; i++ {
		store.UpsertBook(db.Book{WorkID: wid,
			Path:     "/generated/tts-book-2/chapter-00" + string(rune('0'+i)) + ".mp3",
			Filename: "chapter-00" + string(rune('0'+i)) + ".mp3",
			Format:   "mp3", MediaType: "audio", Origin: "tts_kokoro",
			Album: "bm_fable", Edition: "Kokoro · Fable", Duration: 500})
	}
	c, err := BuildWorkCanon(store, wid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Active.AudioFiles != 1 {
		t.Errorf("active edition has %d files — the (shorter) HUMAN narration must win the default, not our (longer) TTS", c.Active.AudioFiles)
	}
}

// The anchor book holding an edition's whole-timeline chapters is not always
// files[0] (the list-projection discovery defect) — canon must name it so
// consumers probe exactly one book, correctly.
func TestWorkCanonNamesChaptersAnchor(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("Anchored", "")
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/a/06.mp3", Filename: "06.mp3",
		Format: "mp3", MediaType: "audio", Origin: "narrator_recording", Duration: 100})
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/a/01.mp3", Filename: "01.mp3",
		Format: "mp3", MediaType: "audio", Origin: "narrator_recording", Duration: 100})
	w, _ := store.GetWork(wid)
	var anchor int64
	for _, b := range w.AudioFiles {
		if b.Filename == "01.mp3" {
			anchor = b.ID
		}
	}
	for i := 0; i < 3; i++ {
		store.InsertChapter(db.Chapter{BookID: anchor, Index: i, Title: "Ch", WordCount: 10,
			StartSec: float64(i) * 50, EndSec: float64(i)*50 + 50})
	}
	c, err := BuildWorkCanon(store, wid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Active.ChaptersAnchorBookID != anchor {
		t.Errorf("anchor = %d, want %d (the book actually holding the chapter rows, not files[0])",
			c.Active.ChaptersAnchorBookID, anchor)
	}
}

// Task 12 rollup: an edition is complete only when EVERY file's producer
// testified complete; any degraded file degrades it; any file with no
// testimony reads unknown — never dressed as complete.
func TestWorkCanonConditionRollup(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("Cond", "")
	for i := 0; i < 2; i++ {
		store.UpsertBook(db.Book{WorkID: wid, Path: "/gen/tts-book-9/chapter-00" + string(rune('0'+i)) + ".mp3",
			Filename: "chapter-00" + string(rune('0'+i)) + ".mp3", Format: "mp3", MediaType: "audio",
			Origin: "tts_kokoro", Album: "v", Duration: 10})
	}
	w, _ := store.GetWork(wid)
	c, _ := BuildWorkCanon(store, wid)
	if c.Editions[0].Condition != "unknown" {
		t.Errorf("no testimony must read unknown, got %q", c.Editions[0].Condition)
	}
	for _, b := range w.AudioFiles {
		store.SetBookCondition(db.BookCondition{BookID: b.ID, State: "complete", Source: "test"})
	}
	c, _ = BuildWorkCanon(store, wid)
	if c.Editions[0].Condition != "complete" {
		t.Errorf("all-complete must read complete, got %q", c.Editions[0].Condition)
	}
	store.SetBookCondition(db.BookCondition{BookID: w.AudioFiles[0].ID, State: "degraded",
		Reason: "generation interrupted", Source: "test"})
	c, _ = BuildWorkCanon(store, wid)
	if c.Editions[0].Condition != "degraded" || c.Editions[0].ConditionReason == "" {
		t.Errorf("degraded file must degrade the edition with reason, got %q/%q",
			c.Editions[0].Condition, c.Editions[0].ConditionReason)
	}
}
