package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// writeDriftSidecar writes a minimal v3 sidecar whose words are the given text.
func writeDriftSidecar(t *testing.T, path, text string) {
	t.Helper()
	type w struct {
		S float64 `json:"s"`
		E float64 `json:"e"`
		W string  `json:"w"`
	}
	var words []w
	for i, tok := range strings.Fields(text) {
		words = append(words, w{S: float64(i), E: float64(i) + 0.5, W: " " + tok})
	}
	body := map[string]any{
		"version": 3, "schema": "abookify-sidecar/v3",
		"duration": float64(len(words)), "words": words,
	}
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, enc, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

func driftWords(prefix string, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix
	}
	return strings.Join(out, " ")
}

// A repaired sidecar that was never imported must be reported, because that is the
// state Atlas Shrugged was in — reader and Q&A serving a decode already superseded
// by a file on disk, with every other detector saying the data was fine.
func TestSidecarDriftCatchesUnimportedRepair(t *testing.T) {
	store := testStoreForLib(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "audiobooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(root, "audiobooks", "book.mp3")
	if err := os.WriteFile(audio, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The sidecar on disk says "repaired"; the database still says "fabricated".
	writeDriftSidecar(t, filepath.Join(root, "audiobooks", "book.stt.json"),
		driftWords("repaired", 400))

	wid, cerr := store.CreateWork("Drifted", "")
	if cerr != nil {
		t.Fatalf("create work: %v", cerr)
	}
	store.UpsertBook(db.Book{WorkID: wid, Path: "/library/audiobooks/book.mp3",
		Filename: "book.mp3", Format: "mp3", MediaType: "audio"})
	store.UpsertBook(db.Book{WorkID: wid, Path: "generated://transcript/work-1",
		Filename: "T", Format: "transcript", MediaType: "text", Origin: "whisper_transcript"})
	books, _ := store.ListBooks()
	var tb int64
	for _, b := range books {
		if b.Format == "transcript" {
			tb = b.ID
		}
	}
	store.InsertChapter(db.Chapter{BookID: tb, Index: 0, Title: "one",
		Content: driftWords("fabricated", 400), WordCount: 400})

	d, err := DetectSidecarDrift(store, root, wid)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if d.State != DriftStale {
		t.Errorf("state = %q, want %q (match %.2f%%) — an unimported repair is invisible "+
			"to every other detector, so this is the only thing that can report it",
			d.State, DriftStale, d.MatchPercent)
	}
}

// The false positive that shipped in the first version: a work with two audio
// editions (a real reading and a TTS one, or two merged works) has two sidecars and
// two transcripts. Pairing sidecar[0] with transcript[0] compared Call of the Wild's
// TTS sidecar against its LibriVox transcript and called a healthy work stale at
// 76%. Each edition must be judged against the transcript it actually produced.
func TestSidecarDriftPairsEditionsNotIndexes(t *testing.T) {
	store := testStoreForLib(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "audiobooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reading", "tts"} {
		if err := os.WriteFile(filepath.Join(root, "audiobooks", name+".mp3"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately CROSSED: edition "reading" holds text B, edition "tts" holds A,
	// so an index-order pairing mismatches both.
	writeDriftSidecar(t, filepath.Join(root, "audiobooks", "reading.stt.json"), driftWords("bravo", 400))
	writeDriftSidecar(t, filepath.Join(root, "audiobooks", "tts.stt.json"), driftWords("alpha", 400))

	wid, cerr := store.CreateWork("Two editions", "")
	if cerr != nil {
		t.Fatalf("create work: %v", cerr)
	}
	store.UpsertBook(db.Book{WorkID: wid, Path: "/library/audiobooks/reading.mp3",
		Filename: "reading.mp3", Format: "mp3", MediaType: "audio"})
	store.UpsertBook(db.Book{WorkID: wid, Path: "/library/audiobooks/tts.mp3",
		Filename: "tts.mp3", Format: "mp3", MediaType: "audio"})
	store.UpsertBook(db.Book{WorkID: wid, Path: "generated://transcript/work-a",
		Filename: "TA", Format: "transcript", MediaType: "text", Origin: "whisper_transcript"})
	store.UpsertBook(db.Book{WorkID: wid, Path: "generated://transcript/work-b",
		Filename: "TB", Format: "transcript", MediaType: "text", Origin: "whisper_transcript"})

	books, _ := store.ListBooks()
	for _, b := range books {
		switch b.Path {
		case "generated://transcript/work-a":
			store.InsertChapter(db.Chapter{BookID: b.ID, Index: 0, Title: "a",
				Content: driftWords("alpha", 400), WordCount: 400})
		case "generated://transcript/work-b":
			store.InsertChapter(db.Chapter{BookID: b.ID, Index: 0, Title: "b",
				Content: driftWords("bravo", 400), WordCount: 400})
		}
	}

	d, err := DetectSidecarDrift(store, root, wid)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if d.Editions != 2 {
		t.Errorf("editions = %d, want 2", d.Editions)
	}
	if d.State != DriftOK {
		t.Errorf("state = %q (match %.2f%%), want %q — both editions match a transcript, "+
			"just not the one at their own index", d.State, d.MatchPercent, DriftOK)
	}
}
