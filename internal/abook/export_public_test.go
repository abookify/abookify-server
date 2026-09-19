package abook

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

// A PUBLIC export is REFUSED unless every bundled source is declared AND
// cleared (with a name); a declaration alone is not enough (Owl Creek). The
// cover is bundled only when its recorded source is a cleared bundled book's
// own art; an OpenLibrary-sourced cover is omitted, never shipped.
func TestExportPublic_RefusesUnclearedAndGatesCover(t *testing.T) {
	dir := t.TempDir()
	store, work := seedWork(t, dir)
	defer store.Close()
	out := filepath.Join(dir, "pub.abook")

	// 1. nothing declared → refused, naming both books
	err := ExportV2(store, work, out, dir, ExportOptions{IncludeAudio: true, Public: true})
	var nc *ErrNotCleared
	if !errors.As(err, &nc) || len(nc.Missing) != 2 {
		t.Fatalf("expected ErrNotCleared naming 2 books, got %v", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatalf("a refused export must not leave a file behind")
	}
	var audioID, textID int64
	for _, b := range work.AudioFiles {
		audioID = b.ID
	}
	for _, b := range work.TextFiles {
		textID = b.ID
	}
	// 2. DECLARED but not cleared (the Owl Creek shape) → still refused
	for _, id := range []int64{audioID, textID} {
		if err := store.SetSourceProvenance(db.SourceProvenance{Scope: "book", RefID: id, Kind: "librivox", Note: "verify before public redistribution"}); err != nil {
			t.Fatal(err)
		}
	}
	err = ExportV2(store, work, out, dir, ExportOptions{IncludeAudio: true, Public: true})
	if !errors.As(err, &nc) || !strings.Contains(err.Error(), "NOT cleared") {
		t.Fatalf("declared-but-uncleared must be refused, got %v", err)
	}
	// 3. clearance without a name → the store refuses the row itself
	if err := store.SetSourceProvenance(db.SourceProvenance{Scope: "book", RefID: audioID, Kind: "librivox", Cleared: true}); err == nil {
		t.Fatalf("a clearance with no cleared_by must be refused")
	}
	// 4. cleared by a named person → export succeeds; cover from OpenLibrary is OMITTED
	coversDir := filepath.Join(dir, "covers")
	os.MkdirAll(coversDir, 0755)
	coverPath := filepath.Join(coversDir, "work-"+itoa64(work.ID)+".jpg")
	os.WriteFile(coverPath, []byte("\xff\xd8fakejpeg"), 0644)
	library.WriteCoverSource(coverPath, library.CoverSource{Source: "openlibrary", Ref: "olid:OL9236546M"})
	for _, id := range []int64{audioID, textID} {
		if err := store.SetSourceProvenance(db.SourceProvenance{Scope: "book", RefID: id, Kind: "librivox", SourceURL: "https://archive.org/x", License: "public domain", Cleared: true, ClearedBy: "tester"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ExportV2(store, work, out, dir, ExportOptions{IncludeAudio: true, Public: true}); err != nil {
		t.Fatalf("cleared export failed: %v", err)
	}
	m, err := ReadManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if m.Publishing == nil || !m.Publishing.Public || len(m.Publishing.Sources) != 2 {
		t.Fatalf("manifest lacks a proper publishing block: %+v", m.Publishing)
	}
	for _, s := range m.Publishing.Sources {
		if !s.Cleared || s.ClearedBy != "tester" {
			t.Errorf("source %d not recorded as cleared by tester: %+v", s.BookID, s)
		}
	}
	if m.Publishing.Cover.Bundled || m.Assets.Cover != "" {
		t.Errorf("an OpenLibrary cover must be OMITTED from a public export: %+v assets.cover=%q", m.Publishing.Cover, m.Assets.Cover)
	}
	// 5. cover whose recorded source is the cleared EPUB → bundled
	library.WriteCoverSource(coverPath, library.CoverSource{Source: "epub", Ref: "book.epub", BookID: textID})
	out2 := filepath.Join(dir, "pub2.abook")
	if err := ExportV2(store, work, out2, dir, ExportOptions{IncludeAudio: true, Public: true}); err != nil {
		t.Fatalf("export with epub cover failed: %v", err)
	}
	m2, _ := ReadManifest(out2)
	if !m2.Publishing.Cover.Bundled || m2.Assets.Cover != "cover.jpg" {
		t.Errorf("a cover that is the cleared EPUB's own art must be bundled: %+v", m2.Publishing.Cover)
	}
	// 6. a non-public export is unchanged: no block, cover bundled as before
	out3 := filepath.Join(dir, "personal.abook")
	if err := ExportV2(store, work, out3, dir, ExportOptions{IncludeAudio: true}); err != nil {
		t.Fatal(err)
	}
	m3, _ := ReadManifest(out3)
	if m3.Publishing != nil || m3.Assets.Cover != "cover.jpg" {
		t.Errorf("personal export must be unchanged: publishing=%v cover=%q", m3.Publishing, m3.Assets.Cover)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
