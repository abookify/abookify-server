package abook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// A multi-file narration keeps each file's place on the book's timeline across
// export → import, and the imported rows come back in timeline order even when
// the source library created them scattered. Without this a fresh install
// lists a human narration's chapters in the exporter's id order (stranger walk,
// 2026-09-21: "11-Lucy Westenra's Diary" first).
func TestImport_PreservesStartSecAndTimelineOrder(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	workID, err := store.CreateWork("Ordered Book", "Ada Author")
	if err != nil {
		t.Fatal(err)
	}
	// Insert in scattered order (11, 04, 01) like a scanned library would.
	files := []struct {
		name  string
		start float64
	}{{"book_11.mp3", 2000}, {"book_04.mp3", 600}, {"book_01.mp3", 0}}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, []byte("ID3 fake "+f.name), 0644); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertBook(db.Book{WorkID: workID, Path: p, Filename: f.name, Format: "mp3", MediaType: "audio",
			Title: f.name, Duration: 300, Origin: "narrator_recording"}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetBookStartSec(bookID(t, store, p), f.start); err != nil {
			t.Fatal(err)
		}
	}
	w, err := store.GetWork(workID)
	if err != nil || w == nil {
		t.Fatalf("get work: %v", err)
	}
	abook := filepath.Join(dir, "ordered.abook")
	if err := ExportV2(store, w, abook, dir, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export: %v", err)
	}

	dest, err := db.Open(filepath.Join(dir, "dest.db"))
	if err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, "lib")
	res, err := ImportInto(dest, abook, lib, ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := dest.GetWork(res.WorkID)
	if err != nil || got == nil {
		t.Fatalf("get imported work: %v", err)
	}
	var audio []db.Book
	for _, b := range got.AudioFiles {
		audio = append(audio, b)
	}
	if len(audio) != 3 {
		t.Fatalf("want 3 audio files, got %d", len(audio))
	}
	// Sort by id (insertion order) and expect the timeline order 01, 04, 11.
	for i := 1; i < len(audio); i++ {
		if audio[i].ID < audio[i-1].ID {
			audio[i], audio[i-1] = audio[i-1], audio[i]
			i = 0
		}
	}
	wantOrder := []string{"book_01.mp3", "book_04.mp3", "book_11.mp3"}
	wantStart := []float64{0, 600, 2000}
	for i, b := range audio {
		if b.Filename != wantOrder[i] {
			t.Errorf("id order[%d] = %s, want %s", i, b.Filename, wantOrder[i])
		}
		if b.StartSec != wantStart[i] {
			t.Errorf("%s start_sec = %v, want %v (dropped on import)", b.Filename, b.StartSec, wantStart[i])
		}
	}
}

// The producer's testimony rides the bundle: a TTS edition the generator marked
// "complete" arrives "complete" on a fresh install, not "unknown".
func TestImport_CarriesBookConditions(t *testing.T) {
	dir := t.TempDir()
	store, w := seedNarration(t, dir, "tts_kokoro", "af_heart")
	var audioID int64
	for _, b := range w.AudioFiles {
		audioID = b.ID
	}
	if err := store.SetBookCondition(db.BookCondition{BookID: audioID, State: "complete", Source: "tts_generate"}); err != nil {
		t.Fatal(err)
	}
	abook := filepath.Join(dir, "cond.abook")
	if err := ExportV2(store, w, abook, dir, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	dest, err := db.Open(filepath.Join(dir, "dest.db"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := ImportInto(dest, abook, filepath.Join(dir, "lib"), ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := dest.GetWork(res.WorkID)
	if err != nil || got == nil {
		t.Fatalf("get work: %v", err)
	}
	var ids []int64
	for _, b := range got.AudioFiles {
		ids = append(ids, b.ID)
	}
	conds, err := dest.GetBookConditions(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(conds) != 1 {
		t.Fatalf("want 1 restored condition, got %d", len(conds))
	}
	for _, c := range conds {
		if c.State != "complete" || c.Source != "tts_generate" {
			t.Errorf("condition = %+v, want complete/tts_generate", c)
		}
	}
}

// Our own samples arrived "Text not checked" (the stranger walk, 2026-09-22).
// The producer knows: a bundle whose audio was all generated from its text
// testifies to that at export — zero suspect words, by construction — and the
// importer carries the verdict. A bundle with a human narration carries the
// work's sweep verdict instead, and never invents one.
func TestImport_CarriesTextTrustTestimony(t *testing.T) {
	dir := t.TempDir()
	store, w := seedNarration(t, dir, "tts_kokoro", "af_heart")
	abook := filepath.Join(dir, "tts.abook")
	if err := ExportV2(store, w, abook, dir, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	dest, err := db.Open(filepath.Join(dir, "dest.db"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := ImportInto(dest, abook, filepath.Join(dir, "lib"), ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	tt, err := dest.GetTextTrust(res.WorkID)
	if err != nil || tt == nil {
		t.Fatalf("a Kokoro-only bundle must arrive with a text-trust verdict (err %v)", err)
	}
	if tt.SuspectWords != 0 || tt.TotalWords == 0 || !tt.HasConfidence {
		t.Errorf("by-construction verdict wrong: %+v", tt)
	}

	// Human narration, no sweep run: nothing is invented.
	dir2 := t.TempDir()
	store2, w2 := seedNarration(t, dir2, "narrator_recording", "")
	abook2 := filepath.Join(dir2, "human.abook")
	if err := ExportV2(store2, w2, abook2, dir2, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	dest2, _ := db.Open(filepath.Join(dir2, "dest.db"))
	res2, err := ImportInto(dest2, abook2, filepath.Join(dir2, "lib"), ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if tt2, _ := dest2.GetTextTrust(res2.WorkID); tt2 != nil {
		t.Errorf("human bundle without a sweep must not carry a verdict, got %+v", tt2)
	}

	// Human narration WITH a sweep verdict: it rides along as recorded.
	if err := store2.SaveTextTrust(db.TextTrustRow{WorkID: w2.ID, CheckedAt: "2026-09-01 00:00:00", HasConfidence: true, SuspectWords: 7, TotalWords: 1000}); err != nil {
		t.Fatal(err)
	}
	abook3 := filepath.Join(dir2, "human-swept.abook")
	if err := ExportV2(store2, w2, abook3, dir2, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	dest3, _ := db.Open(filepath.Join(dir2, "dest3.db"))
	res3, err := ImportInto(dest3, abook3, filepath.Join(dir2, "lib3"), ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if tt3, _ := dest3.GetTextTrust(res3.WorkID); tt3 == nil || tt3.SuspectWords != 7 || tt3.TotalWords != 1000 {
		t.Errorf("sweep verdict not carried: %+v", tt3)
	}
}
