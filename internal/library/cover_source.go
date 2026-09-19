package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// CoverSource records WHERE a work's cover file came from, written as a sidecar
// (`work-N.jpg.source.json`) at the moment the cover is written — by whichever
// path wrote it. It exists because our own fetch-missing backfill pulled two
// modern publishers' cover art from OpenLibrary into the live library seven
// weeks after import, and a public showcase export then shipped them (found
// 2026-09-18, public since 07-04). A personal library may look however its
// owner likes; what we PUBLISH may not. The sidecar is what lets the export
// tell the difference: only a cover whose source is a book the work's
// provenance has CLEARED (or an explicitly cleared cover) is bundled publicly.
// A cover with no sidecar (legacy) is of unknown origin → never bundled publicly.
type CoverSource struct {
	// Source: "epub" | "audio" (embedded in a book the library holds) |
	// "openlibrary" (backfill or picker) | "upload" (user file) | "abook" (imported).
	Source string `json:"source"`
	// Ref names the origin: a book id (epub/audio), an OpenLibrary OLID/URL,
	// the uploaded filename, or the .abook's title.
	Ref       string `json:"ref,omitempty"`
	BookID    int64  `json:"book_id,omitempty"`
	WrittenAt string `json:"written_at"`
	// Publishable is the writer's own verdict on the SOURCE class only:
	// false for openlibrary/upload/unknown (third-party art), true for a cover
	// embedded in a library book — whose real clearance is the BOOK's provenance
	// row, checked at export. It is never a licence claim by itself.
	Publishable bool `json:"publishable_source_class"`
}

func coverSourcePath(coverPath string) string { return coverPath + ".source.json" }

// WriteCoverSource writes the sidecar for coverPath (best-effort; a cover
// without a sidecar is treated as unknown, which is the conservative reading).
func WriteCoverSource(coverPath string, src CoverSource) {
	if src.WrittenAt == "" {
		src.WrittenAt = time.Now().UTC().Format(time.RFC3339)
	}
	src.Publishable = src.Source == "epub" || src.Source == "audio"
	data, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(coverSourcePath(coverPath), data, 0644)
}

// ReadCoverSource returns the sidecar for coverPath, or ok=false when none exists.
func ReadCoverSource(coverPath string) (CoverSource, bool) {
	data, err := os.ReadFile(coverSourcePath(coverPath))
	if err != nil {
		return CoverSource{}, false
	}
	var src CoverSource
	if json.Unmarshal(data, &src) != nil {
		return CoverSource{}, false
	}
	return src, true
}

// RemoveCoverSource drops the sidecar (when a cover is deleted/replaced).
func RemoveCoverSource(coverPath string) { _ = os.Remove(coverSourcePath(coverPath)) }

// WorkCoverPath is the canonical served cover path for a work.
func WorkCoverPath(coversDir string, workID int64) string {
	return filepath.Join(coversDir, "work-"+strconv.FormatInt(workID, 10)+".jpg")
}
