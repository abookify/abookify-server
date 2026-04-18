package library

import (
	"archive/zip"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/dhowden/tag"
	"github.com/pj/abookify/internal/db"
)

// ExtractCover tries to extract cover art from a book file.
// Returns the path to the saved cover image, or empty string if none found.
func ExtractCover(book *db.Book, coversDir string) string {
	os.MkdirAll(coversDir, 0755)

	outPath := filepath.Join(coversDir, fmt.Sprintf("cover-%d.jpg", book.ID))

	// Skip if already extracted
	if _, err := os.Stat(outPath); err == nil {
		return outPath
	}

	switch book.Format {
	case "epub":
		return extractEPUBCover(book.Path, outPath)
	case "mp3", "m4b", "m4a", "flac":
		return extractAudioCover(book.Path, outPath)
	}

	return ""
}

func extractEPUBCover(epubPath, outPath string) string {
	r, err := zip.OpenReader(epubPath)
	if err != nil {
		return ""
	}
	defer r.Close()

	// Look for common cover image patterns
	coverNames := []string{
		"cover.jpg", "cover.jpeg", "cover.png",
		"Cover.jpg", "Cover.jpeg", "Cover.png",
		"images/cover.jpg", "Images/cover.jpg",
		"OEBPS/images/cover.jpg", "OEBPS/Images/cover.jpg",
	}

	// First try exact names
	for _, name := range coverNames {
		for _, f := range r.File {
			if strings.EqualFold(filepath.Base(f.Name), filepath.Base(name)) &&
				strings.Contains(strings.ToLower(f.Name), "cover") {
				if data := readZipEntry(f); data != nil {
					if err := os.WriteFile(outPath, data, 0644); err == nil {
						return outPath
					}
				}
			}
		}
	}

	// Then try any image file with "cover" in the name
	for _, f := range r.File {
		lower := strings.ToLower(f.Name)
		if strings.Contains(lower, "cover") &&
			(strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") || strings.HasSuffix(lower, ".png")) {
			if data := readZipEntry(f); data != nil {
				if err := os.WriteFile(outPath, data, 0644); err == nil {
					return outPath
				}
			}
		}
	}

	// Last resort: first image file
	for _, f := range r.File {
		lower := strings.ToLower(f.Name)
		if strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") || strings.HasSuffix(lower, ".png") {
			if data := readZipEntry(f); data != nil {
				if err := os.WriteFile(outPath, data, 0644); err == nil {
					return outPath
				}
			}
		}
	}

	return ""
}

func readZipEntry(f *zip.File) []byte {
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()

	data := make([]byte, f.UncompressedSize64)
	n, err := rc.Read(data)
	if n > 0 {
		return data[:n]
	}
	return nil
}

func extractAudioCover(audioPath, outPath string) string {
	f, err := os.Open(audioPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	m, err := tag.ReadFrom(f)
	if err != nil {
		return ""
	}

	pic := m.Picture()
	if pic == nil || len(pic.Data) == 0 {
		return ""
	}

	if err := os.WriteFile(outPath, pic.Data, 0644); err != nil {
		return ""
	}

	return outPath
}

// ExtractCoversForWork tries to find cover art for a work from any of its files.
func ExtractCoversForWork(store *db.Store, work *db.Work, coversDir string) {
	coverPath := filepath.Join(coversDir, fmt.Sprintf("work-%d.jpg", work.ID))
	if _, err := os.Stat(coverPath); err == nil {
		return // Already have a cover
	}

	// Try text files first (EPUBs usually have good covers)
	for _, f := range work.TextFiles {
		path := ExtractCover(&f, coversDir)
		if path != "" {
			os.Rename(path, coverPath)
			log.Printf("extracted cover for %q from %s", work.Title, f.Filename)
			return
		}
	}

	// Try audio files
	for _, f := range work.AudioFiles {
		path := ExtractCover(&f, coversDir)
		if path != "" {
			os.Rename(path, coverPath)
			log.Printf("extracted cover for %q from %s", work.Title, f.Filename)
			return
		}
	}
}
