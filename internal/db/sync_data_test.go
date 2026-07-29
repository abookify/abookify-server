package db

import "testing"

// WorkIDsWithSyncData underpins the work-list "Word-level sync" chip: a book
// that has TTS→Whisper word timestamps (sync_data) but no cross-source
// alignment, so no coverage%. It must return the DISTINCT set of work IDs with
// any sync_data row — deduped across chapters — and omit works with none (a
// podcast/lone-EPUB renders nothing, never a false chip).
func TestWorkIDsWithSyncData(t *testing.T) {
	store := testStore(t)

	// No sync_data yet → empty set (not nil-panic, not an error).
	got, err := store.WorkIDsWithSyncData()
	if err != nil {
		t.Fatalf("WorkIDsWithSyncData: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty set, got %v", got)
	}

	// Work 7 has two chapters of sync_data; work 9 has one; work 42 has none.
	for _, r := range []struct {
		work, audio int64
		ch          int
	}{{7, 1, 0}, {7, 1, 1}, {9, 2, 0}} {
		if err := store.SaveSyncData(r.work, r.audio, r.ch, "[]"); err != nil {
			t.Fatalf("save sync_data: %v", err)
		}
	}

	got, err = store.WorkIDsWithSyncData()
	if err != nil {
		t.Fatalf("WorkIDsWithSyncData: %v", err)
	}
	// Work 7 must appear ONCE despite two rows (DISTINCT).
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct work IDs, got %d (%v)", len(got), got)
	}
	if !got[7] || !got[9] {
		t.Errorf("expected work 7 and 9 present, got %v", got)
	}
	if got[42] {
		t.Errorf("work 42 has no sync_data but was reported present")
	}
}
