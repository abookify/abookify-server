package abook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// seedNarration builds a work with ONE narration (origin/voice as given) and the
// same one-chapter EPUB seedWork uses, so two files of the same title differ only
// in their narration — the showcase's human-vs-AI Carol pair.
func seedNarration(t *testing.T, dir, origin, voice string) (*db.Store, *db.Work) {
	t.Helper()
	store, err := db.Open(filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	workID, err := store.CreateWork("Test Book", "Ada Author")
	if err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(dir, origin+".mp3")
	if err := os.WriteFile(audioPath, []byte("ID3 fake audio bytes "+origin), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBook(db.Book{WorkID: workID, Path: audioPath, Filename: origin + ".mp3", Format: "mp3", MediaType: "audio",
		Title: "Chapter 1", Duration: 100, Origin: origin, Album: voice}); err != nil {
		t.Fatal(err)
	}
	audioID := bookID(t, store, audioPath)
	textPath := filepath.Join(dir, "book.epub")
	if err := store.UpsertBook(db.Book{WorkID: workID, Path: textPath, Filename: "book.epub", Format: "epub", MediaType: "text",
		Title: "Test Book", Origin: "publisher_epub"}); err != nil {
		t.Fatal(err)
	}
	textID := bookID(t, store, textPath)
	if err := store.InsertChapter(db.Chapter{BookID: textID, Index: 0, Title: "One", Content: "It was a bright cold day in April.", WordCount: 8}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAlignment(db.Alignment{WorkID: workID, FromBookID: textID, ToBookID: audioID, Unit: "word", Confidence: 0.9, Method: "anchor", Pairs: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSyncData(workID, audioID, 0, `[[0.0,0.5,"It"],[0.5,0.9,"was"]]`); err != nil {
		t.Fatal(err)
	}
	w, err := store.GetWork(workID)
	if err != nil || w == nil {
		t.Fatalf("get work: %v", err)
	}
	return store, w
}

// A same-title second file must become a SECOND EDITION of the existing work:
// its narration added in its OWN directory (canon groups editions by directory),
// the identical EPUB reused (not duplicated), its alignment remapped onto the
// reused text, and the whole thing idempotent on a re-import.
func TestImportInto_SecondNarrationBecomesEdition(t *testing.T) {
	humanDir, ttsDir := t.TempDir(), t.TempDir()
	hs, hw := seedNarration(t, humanDir, "narrator_recording", "")
	humanAbook := filepath.Join(humanDir, "human.abook")
	if err := ExportV2(hs, hw, humanAbook, humanDir, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export human: %v", err)
	}
	hs.Close()
	ts, tw := seedNarration(t, ttsDir, "tts_kokoro", "af_heart")
	ttsAbook := filepath.Join(ttsDir, "tts.abook")
	if err := ExportV2(ts, tw, ttsAbook, ttsDir, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export tts: %v", err)
	}
	ts.Close()

	lib := t.TempDir()
	dest, err := db.Open(filepath.Join(lib, "monolith.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()

	// The newcomer's first tap: a plain import.
	first, err := ImportInto(dest, humanAbook, lib, ImportOptions{})
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if first.Edition != "narrator-recording" {
		t.Errorf("first edition dir = %q, want narrator-recording", first.Edition)
	}
	// The second tap: same title, other narration → into the existing work.
	existingID, _, found, _ := dest.FindWorkByTitleAuthor("Test Book", "Ada Author")
	if !found || existingID != first.WorkID {
		t.Fatalf("dedupe lookup: found=%v id=%d want %d", found, existingID, first.WorkID)
	}
	second, err := ImportInto(dest, ttsAbook, lib, ImportOptions{IntoWorkID: existingID})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Skipped || second.WorkID != existingID || second.Edition != "tts-kokoro-af-heart" {
		t.Errorf("second = %+v", second)
	}
	if second.AddedBooks != 1 || second.ReusedTexts != 1 {
		t.Errorf("second added=%d reused=%d, want 1 audio added + 1 text reused", second.AddedBooks, second.ReusedTexts)
	}

	works, _ := dest.ListWorks()
	if len(works) != 1 {
		t.Fatalf("got %d works, want 1 (a second EDITION, not a duplicate work)", len(works))
	}
	w := works[0]
	if len(w.AudioFiles) != 2 || len(w.TextFiles) != 1 {
		t.Fatalf("audio=%d text=%d, want 2 narrations + 1 shared text", len(w.AudioFiles), len(w.TextFiles))
	}
	// Distinct directories → canon sees two coherent editions.
	if filepath.Dir(w.AudioFiles[0].Path) == filepath.Dir(w.AudioFiles[1].Path) {
		t.Errorf("both narrations in one directory %q — canon would see one edition with two voices", filepath.Dir(w.AudioFiles[0].Path))
	}
	// Both alignments point at the ONE text book.
	aligns, _ := dest.ListAlignmentsForWork(w.ID)
	if len(aligns) != 2 {
		t.Fatalf("alignments = %d, want 2 (one per narration)", len(aligns))
	}
	textID := w.TextFiles[0].ID
	for _, a := range aligns {
		if a.FromBookID != textID && a.ToBookID != textID {
			t.Errorf("alignment %+v does not reference the reused text %d", a, textID)
		}
	}
	if n, _ := dest.ChapterCount(textID); n != 1 {
		t.Errorf("reused text has %d chapters, want 1 (content must not be copied twice)", n)
	}
	// Idempotent: the same narration again is a no-op.
	again, err := ImportInto(dest, ttsAbook, lib, ImportOptions{IntoWorkID: existingID})
	if err != nil || !again.Skipped {
		t.Errorf("re-import: err=%v res=%+v, want Skipped", err, again)
	}
	if works2, _ := dest.ListWorks(); len(works2) != 1 || len(works2[0].AudioFiles) != 2 {
		t.Errorf("after re-import: works=%d audio=%d, want 1/2", len(works2), len(works2[0].AudioFiles))
	}
}
