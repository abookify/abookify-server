package library

import (
	"encoding/json"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// A TTS edition is word-synced BY CONSTRUCTION — no alignment row exists
// because none was needed — yet the reader showed "no audio sync" for the
// clean Carol sample: mode resolution consulted only alignment rows. The TTS
// path must serve word mode with times on the continuous play-order timeline,
// and must refuse (mode none) when the sync word count doesn't match the
// chapter — the construction guarantee is verified, never assumed.
func TestBuildTextSyncTTSByConstruction(t *testing.T) {
	store := testStoreForLib(t)
	wid, err := store.CreateWork("Clean Carol", "")
	if err != nil {
		t.Fatal(err)
	}
	store.UpsertBook(db.Book{WorkID: wid, Path: "/library/ebooks/carol.epub",
		Filename: "carol.epub", Format: "epub", MediaType: "text", Origin: "publisher_epub"})
	store.UpsertBook(db.Book{WorkID: wid, Path: "/generated/tts-book-1/chapter-000.mp3",
		Filename: "chapter-000.mp3", Format: "mp3", MediaType: "audio", Origin: "tts_kokoro", Duration: 50})
	store.UpsertBook(db.Book{WorkID: wid, Path: "/generated/tts-book-1/chapter-001.mp3",
		Filename: "chapter-001.mp3", Format: "mp3", MediaType: "audio", Origin: "tts_kokoro", Duration: 100})
	books, _ := store.ListBooks()
	var epub, ch0, ch1 int64
	for _, b := range books {
		switch b.Filename {
		case "carol.epub":
			epub = b.ID
		case "chapter-000.mp3":
			ch0 = b.ID
		case "chapter-001.mp3":
			ch1 = b.ID
		}
	}
	store.InsertChapter(db.Chapter{BookID: epub, Index: 0, Title: "Title", Content: "one two three", WordCount: 3})
	store.InsertChapter(db.Chapter{BookID: epub, Index: 1, Title: "Stave One", Content: "marley was dead to begin", WordCount: 5})
	mk := func(words ...string) string {
		var ws []SyncWord
		for i, w := range words {
			ws = append(ws, SyncWord{W: w, S: float64(i), E: float64(i) + 0.5})
		}
		b, _ := json.Marshal(ws)
		return string(b)
	}
	store.SaveSyncData(wid, ch0, 0, mk("one", "two", "three"))
	store.SaveSyncData(wid, ch1, 1, mk("marley", "was", "dead", "to", "begin"))

	ts, err := BuildTextSync(store, wid, epub, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Mode != "word" || ts.Method != "tts" {
		t.Fatalf("mode=%s method=%s, want word/tts", ts.Mode, ts.Method)
	}
	wm, err := BuildDisplayWordSync(store, wid, epub, 1)
	if err != nil || len(wm) != 5 {
		t.Fatalf("word map len=%d err=%v, want 5", len(wm), err)
	}
	// Chapter 1 follows chapter 0's 50s file on the continuous timeline.
	if wm[0].S != 50.0 || wm[0].W != "marley" {
		t.Errorf("first word = %q@%.1f, want marley@50.0 (play-order offset)", wm[0].W, wm[0].S)
	}

	// Word-count mismatch = not built from this text: refuse, mode none.
	store.SaveSyncData(wid, ch1, 1, mk("different", "length"))
	ts, _ = BuildTextSync(store, wid, epub, 1)
	if ts.Mode != "none" {
		t.Errorf("mismatched sync must yield mode none, got %s", ts.Mode)
	}
}
