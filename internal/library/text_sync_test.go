package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

// A transcript chapter WITH word timing must report mode=word AND deliver a
// non-empty map; a transcript chapter with NO word timing must report mode=none,
// not a hollow word promise. This is the "mode word but empty map is a lie" fix
// — text-sync and the word-map builder must agree for every transcript chapter.
func TestTranscriptWordModeHonesty(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, err := store.CreateWork("Transcript Work", "Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: "/x/a.mp3", Filename: "a.mp3", Format: "mp3", MediaType: "audio", Title: "Audio",
	}); err != nil {
		t.Fatalf("upsert audio: %v", err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: "/x/t.txt", Filename: "t.txt", Format: "transcript",
		Origin: "whisper_transcript", MediaType: "text", Title: "Transcript",
	}); err != nil {
		t.Fatalf("upsert transcript: %v", err)
	}
	var audioID, transID int64
	books, _ := store.ListBooks()
	for _, b := range books {
		switch b.Path {
		case "/x/a.mp3":
			audioID = b.ID
		case "/x/t.txt":
			transID = b.ID
		}
	}
	// Chapter 0 has narration in [0,100); chapter 1 [100,200) has NONE.
	store.InsertChapter(db.Chapter{BookID: transID, Index: 0, Title: "Ch1", WordCount: 300, StartSec: 0, EndSec: 100})
	store.InsertChapter(db.Chapter{BookID: transID, Index: 1, Title: "Ch2", WordCount: 300, StartSec: 100, EndSec: 200})
	// sync_data (one continuous blob) covers only 0..90s → only chapter 0 has words.
	if err := store.SaveSyncData(workID, audioID, 0, `[{"w":"hello","s":1,"e":2},{"w":"world","s":80,"e":90}]`); err != nil {
		t.Fatalf("save sync: %v", err)
	}

	// Chapter 0: words present → mode=word, non-empty map.
	ts, _ := BuildTextSync(store, workID, transID, 0)
	if ts == nil || ts.Mode != "word" {
		t.Fatalf("ch0: want mode=word, got %+v", ts)
	}
	if wm, _ := BuildDisplayWordSync(store, workID, transID, 0); len(wm) == 0 {
		t.Fatal("ch0: word map must be non-empty when mode=word")
	}

	// Chapter 1: no words in range → honest none, NOT a hollow word promise.
	ts, _ = BuildTextSync(store, workID, transID, 1)
	if ts == nil || ts.Mode != "none" {
		t.Fatalf("ch1: want mode=none (no word timing), got %+v", ts)
	}
	if wm, _ := BuildDisplayWordSync(store, workID, transID, 1); len(wm) != 0 {
		t.Fatalf("ch1: word map must be empty, got %d words", len(wm))
	}
}

// interpFrac must linearly interpolate audio time between aligned-segment
// anchors and clamp outside their range — the basis-robust core of the #210
// paragraph-follow time mapping.
func TestInterpFrac(t *testing.T) {
	anchors := []fracAnchor{{0.0, 10}, {0.5, 20}, {1.0, 40}}
	cases := []struct {
		frac, want float64
	}{
		{-0.2, 10}, // below first → first sec
		{0.0, 10},  // at first
		{0.25, 15}, // midway in first segment
		{0.5, 20},  // at middle anchor
		{0.75, 30}, // midway in second segment
		{1.0, 40},  // at last
		{1.5, 40},  // above last → last sec
	}
	for _, c := range cases {
		if got := interpFrac(anchors, c.frac); got != c.want {
			t.Errorf("interpFrac(%.2f) = %.2f, want %.2f", c.frac, got, c.want)
		}
	}
	if got := interpFrac(nil, 0.5); got != 0 {
		t.Errorf("interpFrac(nil) = %.2f, want 0", got)
	}
}

func TestClamp01(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{{-1, 0}, {0, 0}, {0.5, 0.5}, {1, 1}, {2, 1}} {
		if got := clamp01(c.in); got != c.want {
			t.Errorf("clamp01(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
