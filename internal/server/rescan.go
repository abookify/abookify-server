package server

import (
	"fmt"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/scanner"
)

// RescanResult is the summary returned from Rescan — what the
// Settings "Rescan library" button shows after a manual sweep.
type RescanResult struct {
	Scanned           int `json:"scanned"`            // files the scanner picked up
	Upserted          int `json:"upserted"`           // book rows successfully written
	NewWorks          int `json:"new_works"`          // works created by the matcher this pass
	ChaptersExtracted int `json:"chapters_extracted"` // EPUB+TXT chapter rows written this pass
}

// Rescan walks the library root, ingests anything new, and runs the
// idempotent parts of the boot pipeline so a manual sweep can rescue
// a watcher miss (NFS/sshfs/fast-create-then-write are the usual
// suspects). Safe to re-run — every step short-circuits when state
// already exists:
//
//   - MOBI/AZW3/AZW → sibling EPUB only when the sibling is missing
//   - UpsertBook does ON CONFLICT(path) so unchanged rows stay put
//   - MatchAndCreateWorks only creates works for unassigned books
//   - ImportSidecars skips works that already have sync_data
//   - EPUB/TXT chapter extraction skips books that already have chapters
//
// Series propagation + paragraph population live in the boot path
// only — kept off the manual rescan to keep latency low for the
// common "I dropped a file" case.
//
// Lives in the server package (not library) because library can't
// import scanner — scanner imports library for ExtractMetadata, and
// the cycle isn't worth breaking just to relocate this orchestration.
func Rescan(store *db.Store, libraryRoot string) (RescanResult, error) {
	var res RescanResult

	library.ConvertMobiFilesInDir(libraryRoot)

	results, err := scanner.Scan(libraryRoot)
	if err != nil {
		return res, fmt.Errorf("scan: %w", err)
	}
	res.Scanned = len(results)
	for _, r := range results {
		if err := store.UpsertBook(r); err == nil {
			res.Upserted++
		}
	}

	worksBefore, _ := store.ListWorks()
	if err := library.MatchAndCreateWorks(store); err != nil {
		applog.Warnf("system", "rescan: matching failed: %v", err)
	}
	worksAfter, _ := store.ListWorks()
	if delta := len(worksAfter) - len(worksBefore); delta > 0 {
		res.NewWorks = delta
	}

	library.ImportSidecars(store, libraryRoot)

	allBooks, _ := store.ListBooks()
	for _, b := range allBooks {
		if b.Format != "epub" && b.Format != "txt" {
			continue
		}
		count, _ := store.ChapterCount(b.ID)
		if count > 0 {
			continue
		}
		var chapters []db.Chapter
		switch b.Format {
		case "epub":
			chapters, _ = library.ExtractEPUBChapters(b.Path, b.ID)
		case "txt":
			chapters, _ = library.ExtractTXTChapters(b.Path, b.ID)
		}
		for _, ch := range chapters {
			if err := store.InsertChapter(ch); err == nil {
				res.ChaptersExtracted++
			}
		}
	}

	applog.Infof("system",
		"rescan: scanned=%d upserted=%d new_works=%d chapters_extracted=%d",
		res.Scanned, res.Upserted, res.NewWorks, res.ChaptersExtracted)
	return res, nil
}
