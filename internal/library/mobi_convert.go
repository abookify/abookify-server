package library

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pj/abookify/internal/applog"
)

// mobiSourceExts are the formats we feed through calibre's ebook-convert
// to produce a sibling .epub. Once converted, the EPUB flows through the
// regular chapter-extraction path in scanner + watcher + boot scan.
var mobiSourceExts = map[string]bool{
	".mobi": true, ".azw3": true, ".azw": true,
}

// ConvertMobiToEpub produces a sibling .epub next to a MOBI/AZW3/AZW
// source using calibre's ebook-convert (installed in the server image).
// Returns the sibling EPUB path.
//
// Idempotent: if a sibling .epub already exists, returns its path without
// re-running the converter. This lets the boot scan, the upload rescan,
// and the watcher all blindly call this without coordination.
//
// Requires the `ebook-convert` binary on PATH. The server Dockerfile
// installs the `calibre` package.
func ConvertMobiToEpub(mobiPath string) (string, error) {
	ext := strings.ToLower(filepath.Ext(mobiPath))
	if !mobiSourceExts[ext] {
		return "", fmt.Errorf("not a mobi/azw source: %s", mobiPath)
	}
	epubPath := strings.TrimSuffix(mobiPath, filepath.Ext(mobiPath)) + ".epub"
	if _, err := os.Stat(epubPath); err == nil {
		return epubPath, nil
	}

	start := time.Now()
	cmd := exec.Command("ebook-convert", mobiPath, epubPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Strip noise — the relevant calibre error is usually the last line.
		tail := strings.TrimSpace(string(out))
		if i := strings.LastIndex(tail, "\n"); i >= 0 {
			tail = strings.TrimSpace(tail[i+1:])
		}
		return "", fmt.Errorf("ebook-convert %s: %w (%s)", filepath.Base(mobiPath), err, tail)
	}
	applog.Infof("system", "mobi convert: %s → %s (%s)",
		filepath.Base(mobiPath), filepath.Base(epubPath), time.Since(start).Round(time.Millisecond))
	return epubPath, nil
}

// ConvertMobiFilesInDir walks root and converts every MOBI/AZW3/AZW
// missing a sibling .epub. Per-file errors are logged but don't abort
// the walk — one bad file shouldn't block the rest of a scan.
//
// Called before scanner.Scan at boot and before the upload rescan so the
// resulting EPUBs are picked up by the same pass.
func ConvertMobiFilesInDir(root string) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !mobiSourceExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		if _, err := ConvertMobiToEpub(path); err != nil {
			applog.Warnf("system", "mobi convert: %v", err)
		}
		return nil
	})
}
