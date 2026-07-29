package library

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// ScanWorkSources decodes every audio file of a work and persists the result,
// so the UI can say WHY narration is absent instead of only how much.
//
// Clean results are persisted too: "we looked and it was fine" is the fact that
// lets a book be called complete, and its absence is what must render as
// "unverified" rather than as good news.
//
// libraryRoot maps the stored container path (/library/...) onto the host, the
// same mapping findSidecar uses.
func ScanWorkSources(store *db.Store, libraryRoot string, workID int64) (int, int, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return 0, 0, err
	}

	var ids []int64
	var paths []string
	for _, b := range work.AudioFiles {
		host := hostAudioPath(b.Path, libraryRoot)
		if host == "" {
			// Generated TTS output lives on a volume we cannot stat; it is our
			// own product and regenerable, so it is not a source to vouch for.
			continue
		}
		ids = append(ids, b.ID)
		paths = append(paths, host)
	}
	if len(paths) == 0 {
		return 0, 0, nil
	}

	scans := ScanSourceFiles(paths)
	damaged := 0
	for i, sc := range scans {
		if sc.Damaged() {
			damaged++
			log.Printf("source-scan: work %d %s — %d decode error(s)%s", workID,
				filepath.Base(paths[i]), sc.DecodeErrors,
				map[bool]string{true: " (TRUNCATED — re-acquire, do not repair)"}[sc.Truncated])
		}
		if err := store.SaveSourceScan(db.SourceScanRow{
			BookID:       ids[i],
			DecodeErrors: sc.DecodeErrors,
			Truncated:    sc.Truncated,
			ZeroAt:       sc.ZeroAt,
			ZeroBytes:    sc.ZeroBytes,
		}); err != nil {
			return len(scans), damaged, err
		}
	}
	log.Printf("source-scan: work %d — %d file(s) scanned, %d damaged", workID, len(scans), damaged)
	return len(scans), damaged, nil
}

// hostAudioPath resolves a stored book path to something on this filesystem,
// returning "" when it does not resolve (generated audio on a container volume).
func hostAudioPath(stored, libraryRoot string) string {
	if stored == "" {
		return ""
	}
	if _, err := os.Stat(stored); err == nil {
		return stored
	}
	if i := strings.Index(stored, "/library/"); i >= 0 {
		cand := filepath.Join(libraryRoot, stored[i+len("/library/"):])
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}
