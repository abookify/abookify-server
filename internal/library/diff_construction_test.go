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

// An embedding-only aligned work (cross-translation: no word-unit rows) must
// emit its pair labeled unit=paragraph — pairs:[] here is the same
// empty-vs-none-of-that-kind misread as the TTS case, one method over.
func TestBuildCoverageEmitsEmbeddingOnlyPair(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("Translation", "")
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/tr.epub", Filename: "tr.epub",
		Format: "epub", MediaType: "text", Origin: "publisher_epub"})
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/tr.transcript", Filename: "tr",
		Format: "transcript", MediaType: "text", Origin: "whisper_transcript"})
	w, _ := store.GetWork(wid)
	eb, tr := w.TextFiles[0].ID, w.TextFiles[1].ID
	payload := `{"match_quality":0.82,"aligned_trans_words":80,"trans_words":100,"aligned_ebook_words":70,"ebook_words":100}`
	if err := store.SaveAlignment(db.Alignment{WorkID: wid, FromBookID: eb, ToBookID: tr,
		Unit: "paragraph", Confidence: 0.8, Method: "embedding", Pairs: payload}); err != nil {
		t.Fatal(err)
	}
	cov, err := BuildCoverage(store, wid)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Pairs) != 1 {
		t.Fatalf("want 1 embedding pair, got %d", len(cov.Pairs))
	}
	p := cov.Pairs[0]
	if p.Method != "embedding" || p.Unit != "paragraph" || p.Verdict == nil {
		t.Errorf("pair mislabeled: %+v", p)
	}
}
