package db

import (
	"path/filepath"
	"testing"
)

// WorkConditionsRollup is the work-level standing the task-12 badge reads. The
// rollup must be conservative and honest: degraded if ANY book is degraded,
// else unknown if ANY book lacks producer testimony (absence is a real state —
// most of the library on day one), else complete. Never dress an untested book
// as complete.
func TestWorkConditionsRollup(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	workID, err := store.CreateWork("Cond Book", "Ada Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	// Two books so we can exercise "some testified, some not".
	for _, p := range []string{filepath.Join(dir, "a.mp3"), filepath.Join(dir, "b.epub")} {
		mt := "audio"
		fmtStr := "mp3"
		if filepath.Ext(p) == ".epub" {
			mt, fmtStr = "text", "epub"
		}
		if err := store.UpsertBook(Book{WorkID: workID, Path: p, Filename: filepath.Base(p), Format: fmtStr, MediaType: mt}); err != nil {
			t.Fatalf("upsert %s: %v", p, err)
		}
	}
	w, err := store.GetWork(workID)
	if err != nil || w == nil {
		t.Fatalf("get work: %v", err)
	}
	var books []Book
	books = append(books, w.AudioFiles...)
	books = append(books, w.TextFiles...)
	if len(books) != 2 {
		t.Fatalf("want 2 books, got %d", len(books))
	}

	get := func() WorkConditionRollup {
		m, err := store.WorkConditionsRollup()
		if err != nil {
			t.Fatalf("rollup: %v", err)
		}
		return m[workID]
	}

	// 1) No testimony on either book → unknown (absence is a real state).
	if r := get(); r.Condition != "unknown" {
		t.Errorf("no testimony: got %q, want unknown", r.Condition)
	}

	// 2) One book complete, the other still silent → still unknown (not dressed
	//    as complete just because one producer testified).
	if err := store.SetBookCondition(BookCondition{BookID: books[0].ID, State: "complete", Source: "test"}); err != nil {
		t.Fatalf("set complete: %v", err)
	}
	if r := get(); r.Condition != "unknown" {
		t.Errorf("partial testimony: got %q, want unknown", r.Condition)
	}

	// 3) Both complete → complete.
	if err := store.SetBookCondition(BookCondition{BookID: books[1].ID, State: "complete", Source: "test"}); err != nil {
		t.Fatalf("set complete 2: %v", err)
	}
	if r := get(); r.Condition != "complete" {
		t.Errorf("both complete: got %q, want complete", r.Condition)
	}

	// 4) One degraded → degraded overrides, and the reason surfaces.
	if err := store.SetBookCondition(BookCondition{BookID: books[1].ID, State: "degraded", Reason: "3 of 6 chapters failed", Source: "test"}); err != nil {
		t.Fatalf("set degraded: %v", err)
	}
	r := get()
	if r.Condition != "degraded" {
		t.Errorf("one degraded: got %q, want degraded", r.Condition)
	}
	if r.Reason != "3 of 6 chapters failed" {
		t.Errorf("degraded reason = %q", r.Reason)
	}
}
