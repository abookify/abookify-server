package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A DB created before pairs was declared last: pairs mid-row, no rev column
// (rev arrives by ALTER TABLE, which appends it after pairs — the shape that
// made every meta read walk the whole payload).
func openLegacyAlignmentsDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE alignments (
			id INTEGER PRIMARY KEY AUTOINCREMENT, work_id INTEGER NOT NULL,
			from_book_id INTEGER NOT NULL, to_book_id INTEGER NOT NULL,
			unit TEXT NOT NULL DEFAULT 'word', confidence REAL NOT NULL DEFAULT 0,
			method TEXT NOT NULL DEFAULT '', pairs TEXT NOT NULL DEFAULT '[]',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(from_book_id, to_book_id, unit))`,
		`INSERT INTO alignments (id, work_id, from_book_id, to_book_id, method, pairs, created_at, updated_at)
		 VALUES (1, 10, 100, 101, 'anchor', '{"a":1}', '2026-01-02 03:04:05', '2026-01-02 03:04:05')`,
		`INSERT INTO alignments (id, work_id, from_book_id, to_book_id, method, pairs)
		 VALUES (5, 10, 100, 102, 'embedding', '{"b":2}')`,
		`DELETE FROM alignments WHERE id = 5`,
		`INSERT INTO alignments (id, work_id, from_book_id, to_book_id, method, pairs)
		 VALUES (7, 11, 110, 111, 'anchor', '{"c":3}')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	raw.Close()
	return path
}

func alignmentColumns(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(`PRAGMA table_info(alignments)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	return cols
}

func TestOpenRebuildsAlignmentsWithPairsLast(t *testing.T) {
	path := openLegacyAlignmentsDB(t)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cols := alignmentColumns(t, store)
	if cols[len(cols)-1] != "pairs" {
		t.Fatalf("pairs must be the last column after Open, got order %v", cols)
	}
	if cols[len(cols)-2] != "rev" {
		t.Fatalf("rev must precede pairs, got order %v", cols)
	}

	// Rows, ids, timestamps and payloads survive the copy.
	meta, err := store.ListAlignmentMeta()
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != 2 || meta[0].ID != 1 || meta[1].ID != 7 {
		t.Fatalf("rows after rebuild: %+v", meta)
	}
	if meta[0].Method != "anchor" || meta[0].WorkID != 10 || meta[0].Rev != 0 {
		t.Fatalf("row 1 lost fields: %+v", meta[0])
	}
	if y := meta[0].CreatedAt.Year(); y != 2026 {
		t.Fatalf("row 1 created_at not preserved: %v", meta[0].CreatedAt)
	}
	for _, want := range []struct {
		id    int64
		pairs string
	}{{1, `{"a":1}`}, {7, `{"c":3}`}} {
		got, err := store.GetAlignmentPairs(want.id)
		if err != nil || got != want.pairs {
			t.Fatalf("pairs of %d: %q %v", want.id, got, err)
		}
	}

	// The three lookup indexes come back with the table.
	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='alignments'`)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		have[n] = true
	}
	rows.Close()
	for _, n := range []string{"idx_alignments_work", "idx_alignments_from", "idx_alignments_to"} {
		if !have[n] {
			t.Fatalf("index %s missing after rebuild (have %v)", n, have)
		}
	}

	// AUTOINCREMENT followed the copied ids: the deleted id 5 and the live
	// max 7 are both behind the counter, so a new row never reuses an id the
	// summary cache may still hold.
	res, err := store.db.Exec(`INSERT INTO alignments (work_id, from_book_id, to_book_id, pairs) VALUES (12, 120, 121, '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 8 {
		t.Fatalf("new row got id %d, want 8 (counter must continue past the copied max)", id)
	}

	// Second Open is a no-op: same order, same rows.
	store.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if c := alignmentColumns(t, again); c[len(c)-1] != "pairs" {
		t.Fatalf("second Open changed the order: %v", c)
	}
	if meta, _ := again.ListAlignmentMeta(); len(meta) != 3 {
		t.Fatalf("second Open changed the rows: %+v", meta)
	}
}

// A fresh DB declares pairs last; the rebuild has nothing to do.
func TestFreshAlignmentsHasPairsLast(t *testing.T) {
	store := testStore(t)
	if c := alignmentColumns(t, store); c[len(c)-1] != "pairs" || c[len(c)-2] != "rev" {
		t.Fatalf("fresh order %v", c)
	}
}
