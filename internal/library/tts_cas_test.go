package library

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Different text MUST yield a different key (the defect being removed), and
// the key must be whitespace-insensitive (structural churn never changes it).
func TestTTSContentKey(t *testing.T) {
	a := TTSContentKey("Marley was dead", "bm_fable", 1100, 500)
	if TTSContentKey("Marley was  dead\n", "bm_fable", 1100, 500) != a {
		t.Error("whitespace changed the key — structural churn would invalidate audio")
	}
	if TTSContentKey("Marley was alive", "bm_fable", 1100, 500) == a {
		t.Error("different text, same key — stale audio would be servable")
	}
	if TTSContentKey("Marley was dead", "af_heart", 1100, 500) == a {
		t.Error("different voice, same key")
	}
	if TTSContentKey("Marley was dead", "bm_fable", 1100, 900) == a {
		t.Error("different cadence, same key")
	}
}

// The full chapter lifecycle: assemble in work, promote atomically, link
// into the edition, detect currency by KEY not existence, and GC only what
// nothing references.
func TestCasLifecycleAndGC(t *testing.T) {
	gen := t.TempDir()
	key := TTSContentKey("some words", "v", 1100, 500)
	mp3 := filepath.Join(gen, "tts-book-1", "chapter-000.mp3")
	os.MkdirAll(filepath.Dir(mp3), 0755)

	if CasHasChapter(mp3, key) {
		t.Fatal("empty store claims currency")
	}
	wd, err := CasWorkDir(gen, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	wf := filepath.Join(wd, "ch0.mp3")
	os.WriteFile(wf, []byte("AUDIO"), 0644)
	if err := CasPromote(gen, key, wf, mp3); err != nil {
		t.Fatal(err)
	}
	if !CasHasChapter(mp3, key) {
		t.Fatal("promoted chapter not current")
	}
	// A TEXT CHANGE: new key — the same file must read as STALE.
	key2 := TTSContentKey("different words", "v", 1100, 500)
	if CasHasChapter(mp3, key2) {
		t.Fatal("stale audio reads as current after text change — the defect survived")
	}
	// GC: the linked entry survives; an orphan goes.
	orphan := casPath(gen, TTSContentKey("orphan", "v", 0, 0))
	os.MkdirAll(filepath.Dir(orphan), 0755)
	os.WriteFile(orphan, []byte("X"), 0644)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(orphan, old, old)
	removed, works := CleanTTSCas(gen, 24*time.Hour, map[string]bool{})
	if removed != 1 {
		t.Errorf("GC removed %d entries, want 1 (the orphan only)", removed)
	}
	if works != 1 {
		t.Errorf("GC removed %d work dirs, want 1 (job-1 done)", works)
	}
	if !CasHasChapter(mp3, key) {
		t.Error("GC broke a live edition file")
	}
}
