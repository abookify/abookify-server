package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

// A TTS edition narrating its source text has NO alignments row (word sync is
// by the chapter-file rail), and /coverage used to return pairs:[] for it —
// indistinguishable from "nothing aligned". Mobile read that on the clean
// fixture and concluded it couldn't certify karaoke. The pair must be emitted,
// labeled by_construction.
func TestBuildCoverageEmitsTTSConstructionPair(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("Clean", "")
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/clean.epub", Filename: "clean.epub",
		Format: "epub", MediaType: "text", Origin: "publisher_epub"})
	w, _ := store.GetWork(wid)
	textID := w.TextFiles[0].ID
	store.UpsertBook(db.Book{WorkID: wid, Path: "/gen/tts-book/chapter-000.mp3",
		Filename: "chapter-000.mp3", Format: "mp3", MediaType: "audio",
		Origin: "tts_kokoro", Duration: 3})
	w, _ = store.GetWork(wid)
	audioID := w.AudioFiles[0].ID
	if err := store.InsertChapter(db.Chapter{BookID: textID, Index: 0, Title: "One",
		Content: "one two three", WordCount: 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSyncData(wid, audioID, 0,
		`[{"w":"one","s":0,"e":1},{"w":"two","s":1,"e":2},{"w":"three","s":2,"e":3}]`); err != nil {
		t.Fatal(err)
	}
	cov, err := BuildCoverage(store, wid)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Pairs) != 1 {
		t.Fatalf("want 1 by-construction pair, got %d", len(cov.Pairs))
	}
	p := cov.Pairs[0]
	if !p.ByConstruction || p.Method != "tts_construction" || p.AudioToEbook != 1 {
		t.Errorf("pair not labeled by construction: %+v", p)
	}
	if p.Verdict == nil || p.Verdict.Bucket != VerdictSameEdition {
		t.Errorf("verdict must state same_edition by construction, got %+v", p.Verdict)
	}
}

// A transcript-only work (no TTS chapter files) must NOT gain a fake pair.
func TestBuildCoverageNoConstructionPairWithoutTTS(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("HumanOnly", "")
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/h.epub", Filename: "h.epub",
		Format: "epub", MediaType: "text", Origin: "publisher_epub"})
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/a/01.mp3", Filename: "01.mp3",
		Format: "mp3", MediaType: "audio", Origin: "narrator_recording", Duration: 9})
	cov, err := BuildCoverage(store, wid)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Pairs) != 0 {
		t.Fatalf("want 0 pairs, got %d", len(cov.Pairs))
	}
}
