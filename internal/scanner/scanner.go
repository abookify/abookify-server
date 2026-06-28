package scanner

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

// Supported file extensions mapped to format names.
var supportedFormats = map[string]string{
	".epub": "epub",
	".mobi": "mobi",
	".azw3": "mobi",
	".azw":  "mobi",
	".pdf":  "pdf",
	".txt":  "txt",
	".text": "txt",
	".mp3":  "mp3",
	".m4b":  "m4b",
	".m4a":  "m4a",
	".flac": "flac",
	".aac":  "aac",
	".opus": "opus",
	".ogg":  "opus",
}

var audioFormats = map[string]bool{
	"mp3": true, "m4b": true, "m4a": true, "flac": true, "aac": true, "opus": true,
}

// Scan walks the given directory and returns a Book entry for each
// supported file found. Extracts metadata for every match — use this on
// first-pass scans where the DB is empty.
func Scan(root string) ([]db.Book, error) {
	return scan(root, nil)
}

// ScanIncremental is Scan with a metadata-extraction skip-set. Pass a
// map of path → SizeBytes for books currently in the DB; entries whose
// size matches what we see on disk get omitted from the result entirely
// — the existing DB row is already correct, no UpsertBook needed and
// no expensive ID3/EPUB read either. Size mismatch or absence falls
// through to a normal full extract.
//
// Built for the manual /api/library/rescan path: a 722-file library
// drops from ~50s (every file ID3-read) to <1s (only new/changed files
// extracted).
func ScanIncremental(root string, known map[string]int64) ([]db.Book, error) {
	return scan(root, known)
}

func scan(root string, known map[string]int64) ([]db.Book, error) {
	var results []db.Book

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		format, ok := supportedFormats[ext]
		if !ok {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		// Incremental skip: same path with same size already in DB → the
		// existing row is correct, don't pay for metadata extraction.
		if known != nil {
			if prev, hit := known[path]; hit && prev == info.Size() {
				return nil
			}
		}

		mediaType := "text"
		if audioFormats[format] {
			mediaType = "audio"
		}

		book := db.Book{
			Path:      path,
			Filename:  d.Name(),
			Format:    format,
			MediaType: mediaType,
			SizeBytes: info.Size(),
			Title:     titleFromFilename(d.Name()),
			Origin:    originForFormat(format),
		}

		// Extract metadata from file
		meta, err := library.ExtractMetadata(path)
		if err != nil {
			log.Printf("metadata warning for %s: %v", d.Name(), err)
		} else {
			if meta.Title != "" {
				book.Title = meta.Title
			}
			if meta.Author != "" {
				book.Author = meta.Author
			}
			book.Album = meta.Album
			if meta.Duration > 0 {
				book.Duration = meta.Duration
			}
		}

		results = append(results, book)
		return nil
	})

	return results, err
}

// originForFormat returns the default origin tag for a scanned file. These are
// conservative defaults — the user can upgrade via the metadata editor later
// (e.g. from "narrator_recording" to "author_recording").
func originForFormat(format string) string {
	switch format {
	case "epub":
		return "publisher_epub"
	case "mobi":
		return "publisher_mobi"
	case "pdf":
		return "publisher_pdf"
	case "txt":
		return "user_upload"
	case "mp3", "m4b", "m4a", "flac", "aac":
		return "narrator_recording"
	default:
		return "user_upload"
	}
}

func titleFromFilename(name string) string {
	title := strings.TrimSuffix(name, filepath.Ext(name))
	title = strings.ReplaceAll(title, "_", " ")
	title = strings.ReplaceAll(title, "-", " ")
	return title
}
