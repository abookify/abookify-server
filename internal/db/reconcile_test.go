package db

import (
	"os"
	"path/filepath"
	"testing"
)

// Every class the reporter promises is planted once, and the sweep must name
// each with the right count — and touch nothing. "Completed without error" is
// not evidence; the number moving is.
func TestReconcileStores_EachClassFires(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	lib := filepath.Join(dir, "library")
	gen := filepath.Join(dir, "generated")
	mk := func(p string, size int) string {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	exec(`INSERT INTO library_roots (id, path, label, is_default, position) VALUES (1, ?, 'Library', 1, 0)`, lib)
	exec(`INSERT INTO works (id, title) VALUES (1, 'Kept'), (2, 'Also kept')`)

	// rows → files
	present := mk(filepath.Join(lib, "audiobooks", "kept", "01.mp3"), 10)
	exec(`INSERT INTO books (id, work_id, root_id, path, filename, format, media_type, origin) VALUES (1, 1, 1, ?, '01.mp3', 'mp3', 'audio', 'narrator_recording')`, present)
	exec(`INSERT INTO books (id, work_id, root_id, path, filename, format, media_type, origin) VALUES (2, 1, 1, ?, '02.mp3', 'mp3', 'audio', 'narrator_recording')`, filepath.Join(lib, "audiobooks", "kept", "02.mp3")) // missing
	ttsLive := mk(filepath.Join(gen, "tts-book-9-af-heart", "chapter-000.mp3"), 10)
	exec(`INSERT INTO books (id, work_id, root_id, path, filename, format, media_type, origin) VALUES (3, 2, 0, ?, 'chapter-000.mp3', 'mp3', 'audio', 'tts_kokoro')`, ttsLive)
	exec(`INSERT INTO books (id, work_id, root_id, path, filename, format, media_type, origin) VALUES (4, 2, 0, ?, 'chapter-001.mp3', 'mp3', 'audio', 'tts_kokoro')`, filepath.Join(gen, "tts-book-9-af-heart", "chapter-001.mp3")) // missing, outside every root
	exec(`INSERT INTO books (id, work_id, root_id, path, filename, format, media_type, origin) VALUES (5, 2, 0, 'generated://transcript/work-2', 'transcript', 'transcript', 'text', 'whisper_transcript')`)                        // virtual: never "missing"

	// rows → rows (the boot sweep does not look at these)
	exec(`INSERT INTO playback_positions (work_id, book_id, position_secs) VALUES (1, 999, 12.5)`) // phantom
	exec(`INSERT INTO playback_positions (work_id, book_id, position_secs) VALUES (2, 0, 0)`)      // unset
	exec(`INSERT INTO book_conditions (book_id, state, reason, source) VALUES (998, 'degraded', 'x', 'test')`)
	exec(`INSERT INTO bookmarks (work_id, book_id, type) VALUES (1, 3, 'bookmark')`) // book 3 belongs to work 2 → cross-work

	// files → rows
	mk(filepath.Join(lib, "audiobooks", "never-scanned", "01.mp3"), 10)    // unindexed media
	mk(filepath.Join(lib, "audiobooks", "never-scanned", "cover.jpg"), 10) // not media: ignored
	mk(filepath.Join(lib, "tts-previews", "af_heart.v1.mp3"), 10)          // INFO class
	mk(filepath.Join(lib, "abooks", "Carol.abook"), 10)                    // INFO: archive
	mk(filepath.Join(lib, "abooks", "Carol", "audio", "book-1.mp3"), 4096) // LOW: unpacked, scanner-skipped, invisible
	mk(filepath.Join(gen, "tts-book-7-bm-fable", "chapter-000.mp3"), 2048) // orphan edition dir
	mk(filepath.Join(gen, "tts-book-9-af-heart", "chapter-002.mp3"), 10)   // unreferenced file in a live dir
	mk(filepath.Join(gen, "waveforms", "3.json"), 10)                      // live
	mk(filepath.Join(gen, "waveforms", "777.json"), 10)                    // stale
	// CAS: chapter-000's sidecar names a live object; one object nobody names;
	// one sidecar names a missing object.
	mk(filepath.Join(gen, "cas", "aa", "aa11.mp3"), 10)
	mk(filepath.Join(gen, "cas", "bb", "bb22.mp3"), 10)
	mk(filepath.Join(gen, "tts-book-9-af-heart", "chapter-000.mp3.cas"), 0)
	os.WriteFile(filepath.Join(gen, "tts-book-9-af-heart", "chapter-000.mp3.cas"), []byte("aa11\n"), 0o644)
	os.WriteFile(filepath.Join(gen, "tts-book-9-af-heart", "chapter-002.mp3.cas"), []byte("cc33\n"), 0o644)

	before := treeSnapshot(t, dir)
	rep, err := store.Reconcile(ReconcileOptions{GeneratedDir: gen})
	if err != nil {
		t.Fatal(err)
	}
	if after := treeSnapshot(t, dir); after != before {
		t.Fatalf("reporter changed the filesystem:\n%s\n---\n%s", before, after)
	}
	t.Log("\n" + FormatReconcileReport(rep))

	want := map[string]int{
		"book_file_missing":                   2, // book 2 (under root) + book 4 (outside roots) — reported as two findings with the same class
		"playback_positions.book_id_dangling": 1,
		"playback_positions.book_id_unset":    1,
		"book_conditions.book_id_dangling":    1,
		"bookmarks_cross_work":                1,
		"library_media_unindexed":             1,
		"library_voice_previews":              1,
		"library_abook_archives":              1,
		"library_abooks_unpacked_media":       1,
		"tts_edition_dir_unreferenced":        1,
		"tts_chapter_file_unreferenced":       1,
		"waveform_cache_stale":                1,
		"cas_object_unreferenced":             1,
		"tts_sidecar_dangling_cas_key":        1,
	}
	got := map[string]int{}
	for _, f := range rep.Findings {
		got[f.Class] += f.Count
	}
	for class, n := range want {
		if got[class] != n {
			t.Errorf("%s: want %d, got %d", class, n, got[class])
		}
	}
	for class := range got {
		if _, ok := want[class]; !ok {
			t.Errorf("unexpected finding %s = %d", class, got[class])
		}
	}
	if rep.Clean {
		t.Error("report claims clean with HIGH findings present")
	}
	if len(rep.Roots) != 1 || rep.Roots[0].Suspected || rep.Roots[0].Missing != 1 {
		t.Errorf("root summary wrong: %+v", rep.Roots)
	}
}

// A root whose files are mostly gone is one "unmounted?" finding, not N
// missing books — the guard that keeps a NAS outage from reading as a purge.
func TestReconcileStores_UnmountedRootIsOneFinding(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	lib := filepath.Join(dir, "nas")
	if _, err := store.db.Exec(`INSERT INTO library_roots (id, path, label, is_default, position) VALUES (1, ?, 'NAS', 1, 0)`, lib); err != nil {
		t.Fatal(err)
	}
	store.db.Exec(`INSERT INTO works (id, title) VALUES (1, 'w')`)
	for i := 0; i < 5; i++ {
		store.db.Exec(`INSERT INTO books (work_id, root_id, path, filename, format, media_type) VALUES (1, 1, ?, 'f', 'mp3', 'audio')`,
			filepath.Join(lib, "b", "0"+string(rune('0'+i))+".mp3"))
	}
	rep, err := store.Reconcile(ReconcileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var unreach, missing int
	for _, f := range rep.Findings {
		switch f.Class {
		case "library_root_unreachable_or_partial":
			unreach += f.Count
		case "book_file_missing":
			missing += f.Count
		}
	}
	if unreach != 1 || missing != 0 {
		t.Fatalf("want one unreachable-root finding and zero per-book findings, got %d / %d\n%s", unreach, missing, FormatReconcileReport(rep))
	}
}

func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var out string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if filepath.Ext(p) == ".db" || filepath.Ext(p) == ".db-wal" || filepath.Ext(p) == ".db-shm" {
			return nil // the store's own files move on their own
		}
		out += p + " " + info.Mode().String() + "\n"
		if !info.IsDir() {
			out += "  " + string(rune(info.Size())) + "\n"
		}
		return nil
	})
	return out
}

// The boot sweep must take what the reconciler reports as rows_without_rows
// for the tables it owns — a cleared provenance declaration whose book is
// gone is a gate hazard, not debris.
func TestCleanupOrphanedRows_SweepsProvenanceScansAndTrust(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO works (id, title) VALUES (1, 'kept')`)
	exec(`INSERT INTO books (id, work_id, path, filename, format, media_type) VALUES (1, 1, '/x/a.mp3', 'a', 'mp3', 'audio')`)
	exec(`INSERT INTO source_provenance (scope, ref_id, kind, source_url, license, cleared) VALUES ('book', 1, 'librivox', '', '', 1), ('book', 999, 'kokoro', '', '', 1), ('cover', 1, 'x', '', '', 1), ('cover', 777, 'x', '', '', 1)`)
	exec(`INSERT INTO source_scans (book_id) VALUES (1), (998)`)
	exec(`INSERT INTO text_trust (work_id) VALUES (1), (555)`)
	n, err := store.CleanupOrphanedRows()
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("want 4 rows swept (book 999, cover 777, scan 998, trust 555), got %d", n)
	}
	var left int
	store.db.QueryRow(`SELECT count(*) FROM source_provenance`).Scan(&left)
	if left != 2 {
		t.Errorf("provenance rows left = %d, want 2 (the live book + the live cover)", left)
	}
	rep, err := store.Reconcile(ReconcileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Direction == "rows_without_rows" {
			t.Errorf("reconciler still sees %s after the sweep", f.Class)
		}
	}
}

func TestUpdateChapterText_CarriesTitle(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.db.Exec(`INSERT INTO works (id, title) VALUES (1, 'w')`)
	store.db.Exec(`INSERT INTO books (id, work_id, path, filename, format, media_type) VALUES (1, 1, '/x.epub', 'x', 'epub', 'text')`)
	store.db.Exec(`INSERT INTO chapters (book_id, index_num, title, content, word_count, start_sec) VALUES (1, 0, 'I.', 'old', 1, 12.5)`)
	if err := store.UpdateChapterText(1, 0, "I.\n\nA SCANDAL IN BOHEMIA", "new words", "<p>new words</p>", 2); err != nil {
		t.Fatal(err)
	}
	var title, content string
	var start float64
	store.db.QueryRow(`SELECT title, content, start_sec FROM chapters WHERE book_id=1 AND index_num=0`).Scan(&title, &content, &start)
	if title != "I.\n\nA SCANDAL IN BOHEMIA" || content != "new words" || start != 12.5 {
		t.Errorf("got title %q content %q start %v", title, content, start)
	}
}
