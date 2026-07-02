package abook

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// Chunk embeddings must travel in the .abook when IncludeEmbeddings is set
// (so a downloaded book supports on-device cosine), and stay out when it isn't.
// The manifest's has_embeddings flag mirrors the choice.
func TestExportV2_Embeddings(t *testing.T) {
	emb := make([]byte, 1536*4) // 1536-dim float32 → text-embedding-3-small
	for i := range emb {
		emb[i] = byte(i % 251)
	}

	check := func(t *testing.T, include bool) {
		dir := t.TempDir()
		store, work := seedWork(t, dir)
		defer store.Close()
		textID := bookID(t, store, filepath.Join(dir, "book.epub"))
		if err := store.InsertChunk(db.Chunk{
			BookID: textID, ChapterIdx: 0, ChunkIdx: 0,
			Content: "It was a bright cold day in April.", StartWord: 0, EndWord: 8,
			Embedding: emb,
		}); err != nil {
			t.Fatalf("insert chunk: %v", err)
		}

		out := filepath.Join(dir, "test.abook")
		if err := ExportV2(store, work, out, dir, ExportOptions{IncludeAudio: false, IncludeEmbeddings: include}); err != nil {
			t.Fatalf("export: %v", err)
		}

		// manifest flag mirrors the option.
		zr, err := zip.OpenReader(out)
		if err != nil {
			t.Fatalf("open zip: %v", err)
		}
		mdata, _ := readFromZip(&zr.Reader, "manifest.json")
		zr.Close()
		var m Manifest
		json.Unmarshal(mdata, &m)
		if m.HasEmbeddings != include {
			t.Errorf("manifest has_embeddings = %v, want %v", m.HasEmbeddings, include)
		}
		if include {
			if m.EmbeddingDim != 1536 || m.EmbeddingModel != "text-embedding-3-small" {
				t.Errorf("embedding model/dim = %q/%d, want text-embedding-3-small/1536", m.EmbeddingModel, m.EmbeddingDim)
			}
		} else if m.EmbeddingDim != 0 || m.EmbeddingModel != "" {
			t.Errorf("omitted: embedding model/dim = %q/%d, want empty", m.EmbeddingModel, m.EmbeddingDim)
		}

		// Round-trip import and inspect the carried chunk's embedding.
		destDir := t.TempDir()
		dest, err := db.Open(filepath.Join(destDir, "monolith.db"))
		if err != nil {
			t.Fatalf("open dest: %v", err)
		}
		defer dest.Close()
		if err := Import(dest, out, destDir); err != nil {
			t.Fatalf("import: %v", err)
		}
		works, _ := dest.ListWorks()
		if len(works) != 1 {
			t.Fatalf("got %d works", len(works))
		}
		var tid int64
		for _, tf := range works[0].TextFiles {
			tid = tf.ID
		}
		chunks, err := dest.ListChunks(tid)
		if err != nil || len(chunks) != 1 {
			t.Fatalf("chunks = %v (err %v)", len(chunks), err)
		}
		got := chunks[0].Embedding
		if include {
			if len(got) != len(emb) || string(got) != string(emb) {
				t.Errorf("embedding = %v, want %v (carried)", got, emb)
			}
		} else if len(got) != 0 {
			t.Errorf("embedding = %v, want empty (omitted)", got)
		}
	}

	t.Run("included", func(t *testing.T) { check(t, true) })
	t.Run("omitted", func(t *testing.T) { check(t, false) })
}

// The original ebook source file bundles under originals/ by default, and the
// manifest advertises it (has_original_ebook + originals list + minor v1).
func TestExportV2_OriginalsBundled(t *testing.T) {
	dir := t.TempDir()
	store, work := seedWork(t, dir)
	defer store.Close()
	// seedWork registers a text book at dir/book.epub but doesn't write it;
	// write real bytes so the export bundles the original.
	epubBytes := []byte("PK\x03\x04 fake epub bytes for fidelity")
	if err := os.WriteFile(filepath.Join(dir, "book.epub"), epubBytes, 0o644); err != nil {
		t.Fatalf("write epub: %v", err)
	}

	out := filepath.Join(dir, "test.abook")
	if err := ExportV2(store, work, out, dir, ExportOptions{IncludeAudio: false}); err != nil {
		t.Fatalf("export: %v", err)
	}

	info, err := Inspect(out)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	m := info.Manifest
	if m.MinorVersion != 1 {
		t.Errorf("minor_version = %d, want 1", m.MinorVersion)
	}
	if m.GeneratedAt == "" {
		t.Error("generated_at is empty, want an export timestamp")
	}
	// seedWork = narrator audio + anchored-dp/word alignment + publisher epub.
	if !strings.Contains(m.Provenance, "narrated audio") ||
		!strings.Contains(m.Provenance, "alignment") ||
		!strings.Contains(m.Provenance, "publisher ebook") {
		t.Errorf("provenance = %q, want narrated audio · …alignment · publisher ebook", m.Provenance)
	}
	if !m.HasOriginalEbook || len(m.Originals) != 1 {
		t.Fatalf("has_original_ebook=%v originals=%+v, want 1 bundled", m.HasOriginalEbook, m.Originals)
	}
	if m.Originals[0].Format != "epub" || m.Originals[0].Path != "originals/book.epub" {
		t.Errorf("original = %+v", m.Originals[0])
	}
	if m.HasAudio { // audio omitted for this export
		t.Error("has_audio should be false (IncludeAudio: false)")
	}
	// The bundled file must be present + byte-identical.
	var found bool
	for _, f := range info.Files {
		if f.Name == "originals/book.epub" {
			found = true
			if f.Size != int64(len(epubBytes)) {
				t.Errorf("bundled original size = %d, want %d", f.Size, len(epubBytes))
			}
		}
	}
	if !found {
		t.Errorf("originals/book.epub not in the archive; files = %v", info.Files)
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
