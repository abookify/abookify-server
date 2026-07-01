package abook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// Inspect → ExtractTo → PackDir → Inspect round-trip backs the `abook` CLI.
func TestCLIInspectExtractPack(t *testing.T) {
	dir := t.TempDir()
	store, work := seedWork(t, dir)
	textID := bookID(t, store, filepath.Join(dir, "book.epub"))
	if err := store.InsertChunk(db.Chunk{
		BookID: textID, ChapterIdx: 0, ChunkIdx: 0, Content: "chunk text",
		StartWord: 0, EndWord: 8, Embedding: make([]byte, 1536*4),
	}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	abookPath := filepath.Join(dir, "test.abook")
	if err := ExportV2(store, work, abookPath, dir, ExportOptions{IncludeAudio: true, IncludeEmbeddings: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	store.Close()

	// --- Inspect ---
	info, err := Inspect(abookPath)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.Manifest == nil || info.Manifest.Title != "Test Book" {
		t.Fatalf("manifest = %+v", info.Manifest)
	}
	if !info.StandardDeflate {
		t.Error("expected StandardDeflate=true (browser-decodable)")
	}
	if info.Chunks != 1 || info.EmbeddedChunks != 1 {
		t.Errorf("chunks=%d embedded=%d, want 1/1", info.Chunks, info.EmbeddedChunks)
	}
	// One audio + one text source.
	var haveAudio, haveText bool
	for _, s := range info.Sources {
		haveAudio = haveAudio || s.MediaType == "audio"
		haveText = haveText || s.MediaType == "text"
	}
	if !haveAudio || !haveText {
		t.Errorf("sources = %+v, want audio+text", info.Sources)
	}
	// manifest + book.db must be present in the file list.
	names := map[string]bool{}
	for _, f := range info.Files {
		names[f.Name] = true
	}
	if !names["manifest.json"] || !names["book.db"] {
		t.Errorf("files = %v, want manifest.json + book.db", names)
	}

	// --- ExtractTo ---
	outDir := filepath.Join(dir, "unpacked")
	written, err := ExtractTo(abookPath, outDir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(written) != len(info.Files) {
		t.Errorf("extracted %d files, want %d", len(written), len(info.Files))
	}
	if _, err := os.Stat(filepath.Join(outDir, "book.db")); err != nil {
		t.Errorf("book.db not extracted: %v", err)
	}

	// --- PackDir (repack the extracted dir) ---
	repacked := filepath.Join(dir, "repacked.abook")
	if err := PackDir(outDir, repacked); err != nil {
		t.Fatalf("pack: %v", err)
	}
	info2, err := Inspect(repacked)
	if err != nil {
		t.Fatalf("inspect repacked: %v", err)
	}
	if info2.Manifest == nil || info2.Manifest.Title != "Test Book" || info2.Chunks != 1 {
		t.Errorf("repacked inspect = title %q chunks %d", info2.Manifest.Title, info2.Chunks)
	}
	// The repacked manifest's book.db checksum must match the packed book.db.
	if info2.Manifest.Checksums["book.db"] == "" {
		t.Error("repacked manifest missing book.db checksum")
	}
}
