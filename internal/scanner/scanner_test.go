package scanner

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// touch creates a file at path with `size` bytes of zero content. The
// rescan skip-set is keyed on size, so size is the part we care about.
func touch(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			t.Fatalf("truncate %s: %v", path, err)
		}
	}
}

// TestScanIncremental_AllNew is the empty-known-map case: every
// supported file in the tree must come back.
func TestScanIncremental_AllNew(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a.epub"), 100)
	touch(t, filepath.Join(root, "b.mp3"), 200)
	touch(t, filepath.Join(root, "ignore.bin"), 50) // unsupported ext

	books, err := ScanIncremental(root, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("want 2 books, got %d", len(books))
	}
}

// TestScanIncremental_SkipsTTSPreviews: server-owned cache dirs under the
// library root (tts-previews) must not be ingested as audiobooks.
func TestScanIncremental_SkipsTTSPreviews(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "book.mp3"), 100)
	touch(t, filepath.Join(root, "tts-previews", "af_heart.v1.mp3"), 40)
	touch(t, filepath.Join(root, "tts-previews", "am_michael.v1.mp3"), 40)

	books, err := ScanIncremental(root, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(books) != 1 || filepath.Base(books[0].Path) != "book.mp3" {
		t.Fatalf("want only book.mp3, got %d books: %+v", len(books), books)
	}
}

// TestScanIncremental_UnchangedSkipped is the core skip-set assertion:
// a file already in the DB with matching size must NOT be returned —
// the existing row is correct, no metadata re-extraction needed.
func TestScanIncremental_UnchangedSkipped(t *testing.T) {
	root := t.TempDir()
	pathA := filepath.Join(root, "a.epub")
	pathB := filepath.Join(root, "b.mp3")
	touch(t, pathA, 100)
	touch(t, pathB, 200)

	known := map[string]int64{
		pathA: 100,
		pathB: 200,
	}
	books, err := ScanIncremental(root, known)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(books) != 0 {
		t.Fatalf("want 0 (all unchanged), got %d: %+v", len(books), books)
	}
}

// TestScanIncremental_SizeMismatchReturned: a known path whose on-disk
// size has changed must be returned so the DB row gets a fresh
// metadata pass. This is the case where the user edited a file in
// place — common with chapter-merged EPUBs.
func TestScanIncremental_SizeMismatchReturned(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "edited.epub")
	touch(t, path, 200)

	known := map[string]int64{path: 100} // remembered size differs from disk
	books, err := ScanIncremental(root, known)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("want 1 (size mismatch must re-extract), got %d", len(books))
	}
	if books[0].SizeBytes != 200 {
		t.Errorf("returned book should reflect on-disk size 200, got %d", books[0].SizeBytes)
	}
}

// TestScanIncremental_MixedKnownUnknown is the realistic case: most of
// the library is unchanged but one or two files are new. Only the new
// ones come back.
func TestScanIncremental_MixedKnownUnknown(t *testing.T) {
	root := t.TempDir()
	old1 := filepath.Join(root, "old1.epub")
	old2 := filepath.Join(root, "old2.epub")
	new1 := filepath.Join(root, "new1.epub")
	new2 := filepath.Join(root, "sub", "new2.mp3")
	touch(t, old1, 100)
	touch(t, old2, 200)
	touch(t, new1, 300)
	touch(t, new2, 400)

	known := map[string]int64{
		old1: 100,
		old2: 200,
	}
	books, err := ScanIncremental(root, known)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := []string{}
	for _, b := range books {
		got = append(got, b.Path)
	}
	sort.Strings(got)
	want := []string{new2, new1} // new2 sorts first alphabetically
	sort.Strings(want)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// TestScanIncremental_UnsupportedExtIgnored: regardless of the known
// map, an unsupported extension must never appear in the result.
func TestScanIncremental_UnsupportedExtIgnored(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "x.bin"), 10)
	touch(t, filepath.Join(root, "y.zip"), 10)
	touch(t, filepath.Join(root, "ok.epub"), 10)

	books, err := ScanIncremental(root, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(books) != 1 || filepath.Ext(books[0].Filename) != ".epub" {
		t.Fatalf("want 1 epub only, got %+v", books)
	}
}

// TestScan_DelegatesToScanIncremental_NilKnown: Scan must behave like
// ScanIncremental(root, nil) — same files, no skips. Guards against a
// future refactor accidentally diverging the two surfaces.
func TestScan_DelegatesToScanIncremental_NilKnown(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a.epub"), 10)
	touch(t, filepath.Join(root, "b.mp3"), 20)

	a, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	b, err := ScanIncremental(root, nil)
	if err != nil {
		t.Fatalf("ScanIncremental: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("Scan and ScanIncremental(nil) disagree on count: %d vs %d", len(a), len(b))
	}
}
