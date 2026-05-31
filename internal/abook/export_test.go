package abook

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// bookID looks up a book's server id by its unique path.
func bookID(t *testing.T, store *db.Store, path string) int64 {
	t.Helper()
	books, err := store.ListBooks()
	if err != nil {
		t.Fatalf("list books: %v", err)
	}
	for _, b := range books {
		if b.Path == path {
			return b.ID
		}
	}
	t.Fatalf("book not found for path %q", path)
	return 0
}

// seedWork creates a small aligned work (one audio file on disk, one text
// source with a chapter, an alignment, a sync row, and a bookmark) and
// returns the loaded work plus the store.
func seedWork(t *testing.T, dir string) (*db.Store, *db.Work) {
	t.Helper()
	store, err := db.Open(filepath.Join(dir, "monolith.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	workID, err := store.CreateWork("Test Book", "Ada Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}

	// Real audio file so the exporter bundles it.
	audioPath := filepath.Join(dir, "chapter01.mp3")
	if err := os.WriteFile(audioPath, []byte("ID3 fake audio bytes"), 0644); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: audioPath, Filename: "chapter01.mp3",
		Format: "mp3", MediaType: "audio", Title: "Chapter 1",
		Duration: 123.4, Origin: "narrator_recording",
	}); err != nil {
		t.Fatalf("upsert audio: %v", err)
	}
	audioID := bookID(t, store, audioPath)

	textPath := filepath.Join(dir, "book.epub")
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: textPath, Filename: "book.epub",
		Format: "epub", MediaType: "text", Title: "Test Book",
		Origin: "publisher_epub",
	}); err != nil {
		t.Fatalf("upsert text: %v", err)
	}
	textID := bookID(t, store, textPath)

	if err := store.InsertChapter(db.Chapter{
		BookID: textID, Index: 0, Title: "Chapter 1",
		Content: "It was a bright cold day in April.", ContentHTML: "<p>It was a bright cold day in April.</p>",
		WordCount: 8,
	}); err != nil {
		t.Fatalf("insert chapter: %v", err)
	}

	if err := store.SaveAlignment(db.Alignment{
		WorkID: workID, FromBookID: audioID, ToBookID: textID,
		Unit: "word", Confidence: 0.92, Method: "anchored-dp", Pairs: `[{"fc":0,"fs":0,"fe":8,"tc":0,"ts":0,"te":8,"c":0.92}]`,
	}); err != nil {
		t.Fatalf("save alignment: %v", err)
	}

	if err := store.SaveSyncData(workID, audioID, 0, `[[0.0,0.5,"It"],[0.5,0.9,"was"]]`); err != nil {
		t.Fatalf("save sync: %v", err)
	}

	if _, err := store.CreateBookmark(db.Bookmark{
		WorkID: workID, BookID: textID, Type: "highlight", ChapterIdx: 0,
		StartWord: 0, EndWord: 2, TextSnippet: "It was", Color: "#f0a500",
	}); err != nil {
		t.Fatalf("create bookmark: %v", err)
	}

	if err := store.StampVersions(workID, BookDBSchemaVersion); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		t.Fatalf("get work: %v", err)
	}
	return store, work
}

func TestExportV2_ManifestAndAssets(t *testing.T) {
	dir := t.TempDir()
	store, work := seedWork(t, dir)
	defer store.Close()

	out := filepath.Join(dir, "test.abook")
	if err := ExportV2(store, work, out, dir, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export: %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"manifest.json", "book.db", "audio/book-" + itoa(bookID(t, store, filepath.Join(dir, "chapter01.mp3"))) + ".mp3"} {
		if !names[want] {
			t.Errorf("missing zip entry %q (have %v)", want, names)
		}
	}

	data, err := readFromZip(&zr.Reader, "manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.Version != 2 {
		t.Errorf("version = %d, want 2", m.Version)
	}
	if m.WorkID != work.ID {
		t.Errorf("work_id = %d, want %d", m.WorkID, work.ID)
	}
	if m.SourceKind != "aligned" {
		t.Errorf("source_kind = %q, want aligned", m.SourceKind)
	}
	if m.SchemaVersion != BookDBSchemaVersion {
		t.Errorf("schema_version = %d, want %d", m.SchemaVersion, BookDBSchemaVersion)
	}
	if m.CoveragePct == nil || *m.CoveragePct < 91 || *m.CoveragePct > 93 {
		t.Errorf("coverage_pct = %v, want ~92", m.CoveragePct)
	}
	if m.Checksums["book.db"] == "" {
		t.Errorf("missing book.db checksum")
	}
}

func TestExportV2_RoundTripImport(t *testing.T) {
	dir := t.TempDir()
	srcStore, work := seedWork(t, dir)

	out := filepath.Join(dir, "test.abook")
	if err := ExportV2(srcStore, work, out, dir, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	srcStore.Close()

	// Import into a fresh library.
	destDir := t.TempDir()
	destStore, err := db.Open(filepath.Join(destDir, "monolith.db"))
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer destStore.Close()

	if err := Import(destStore, out, destDir); err != nil {
		t.Fatalf("import: %v", err)
	}

	works, err := destStore.ListWorks()
	if err != nil {
		t.Fatalf("list works: %v", err)
	}
	if len(works) != 1 {
		t.Fatalf("got %d works, want 1", len(works))
	}
	w := works[0]
	if w.Title != "Test Book" || w.Author != "Ada Author" {
		t.Errorf("work meta = %q/%q", w.Title, w.Author)
	}
	if !w.HasAudio || !w.HasText {
		t.Errorf("expected audio+text, got audio=%v text=%v", w.HasAudio, w.HasText)
	}

	// Chapter content survived the round trip.
	var textBookID int64
	for _, tf := range w.TextFiles {
		textBookID = tf.ID
	}
	ch, err := destStore.GetChapterContent(textBookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("get chapter: %v", err)
	}
	if ch.Content != "It was a bright cold day in April." {
		t.Errorf("chapter content = %q", ch.Content)
	}

	// Alignment + bookmark survived.
	aligns, _ := destStore.ListAlignmentsForWork(w.ID)
	if len(aligns) != 1 || aligns[0].Method != "anchored-dp" {
		t.Errorf("alignments = %+v", aligns)
	}
	bms, _ := destStore.ListBookmarks(w.ID)
	if len(bms) != 1 || bms[0].Type != "highlight" {
		t.Errorf("bookmarks = %+v", bms)
	}
}

// itoa is a tiny local int64→string to avoid pulling strconv into the test
// just for one call.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
