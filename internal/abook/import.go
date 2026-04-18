package abook

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// Import reads an .abook file and ingests it into the library.
func Import(store *db.Store, abookPath string, libraryDir string) error {
	r, err := zip.OpenReader(abookPath)
	if err != nil {
		return fmt.Errorf("open abook: %w", err)
	}
	defer r.Close()

	// Read manifest
	manifestData, err := readFromZip(&r.Reader, "manifest.json")
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	if manifest.Format != "abook" {
		return fmt.Errorf("not an abook file (format: %q)", manifest.Format)
	}

	log.Printf("abook import: %q by %s (%d chapters)", manifest.Title, manifest.Author, len(manifest.Chapters))

	// Create output directory
	safeName := sanitizeFilename(manifest.Title)
	outDir := filepath.Join(libraryDir, "abooks", safeName)
	os.MkdirAll(filepath.Join(outDir, "text"), 0755)
	os.MkdirAll(filepath.Join(outDir, "audio"), 0755)
	os.MkdirAll(filepath.Join(outDir, "sync"), 0755)

	// Extract all files
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		destPath := filepath.Join(outDir, f.Name)
		os.MkdirAll(filepath.Dir(destPath), 0755)

		rc, err := f.Open()
		if err != nil {
			continue
		}

		out, err := os.Create(destPath)
		if err != nil {
			rc.Close()
			continue
		}

		io.Copy(out, rc)
		out.Close()
		rc.Close()
	}

	// Create a work
	workID, err := store.CreateWork(manifest.Title, manifest.Author)
	if err != nil {
		return fmt.Errorf("create work: %w", err)
	}

	// Register files and chapters
	for _, ch := range manifest.Chapters {
		// Register audio file if present
		if ch.Audio != "" {
			audioPath := filepath.Join(outDir, ch.Audio)
			info, err := os.Stat(audioPath)
			if err == nil {
				ext := filepath.Ext(audioPath)
				format := strings.TrimPrefix(ext, ".")
				book := db.Book{
					WorkID:    workID,
					Path:      audioPath,
					Filename:  filepath.Base(audioPath),
					Format:    format,
					MediaType: "audio",
					SizeBytes: info.Size(),
					Title:     ch.Title,
					Author:    manifest.Author,
				}
				store.UpsertBook(book)
			}
		}

		// Register text as chapters of a virtual text book
		if ch.Text != "" {
			textPath := filepath.Join(outDir, ch.Text)
			data, err := os.ReadFile(textPath)
			if err == nil {
				// We'll create one "text book" per abook import
				textBookPath := filepath.Join(outDir, "text-book.abook-text")
				ensureTextBook(store, workID, textBookPath, manifest.Title, manifest.Author)

				// Find the text book ID
				books, _ := store.ListBooks()
				for _, b := range books {
					if b.Path == textBookPath {
						store.InsertChapter(db.Chapter{
							BookID:    b.ID,
							Index:     ch.Index,
							Title:     ch.Title,
							Content:   stripHTML(string(data)),
							WordCount: ch.WordCount,
						})
						break
					}
				}
			}
		}
	}

	log.Printf("abook import: completed %q (%d chapters)", manifest.Title, len(manifest.Chapters))
	return nil
}

func ensureTextBook(store *db.Store, workID int64, path, title, author string) {
	// Check if already exists
	books, _ := store.ListBooks()
	for _, b := range books {
		if b.Path == path {
			return
		}
	}

	store.UpsertBook(db.Book{
		WorkID:    workID,
		Path:      path,
		Filename:  filepath.Base(path),
		Format:    "abook-text",
		MediaType: "text",
		Title:     title,
		Author:    author,
	})
}

func readFromZip(r *zip.Reader, name string) ([]byte, error) {
	for _, f := range r.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("file %q not found in archive", name)
}

func stripHTML(html string) string {
	// Simple HTML to text — reuse the approach from epub.go
	// Remove tags, decode basic entities
	result := html
	// Remove everything between < and >
	inTag := false
	var sb strings.Builder
	for _, r := range result {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			sb.WriteRune(' ')
			continue
		}
		if !inTag {
			sb.WriteRune(r)
		}
	}
	text := sb.String()
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	// Collapse whitespace
	fields := strings.Fields(text)
	return strings.Join(fields, " ")
}

func sanitizeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '-'
		}
		return r
	}, s)
	if len(s) > 100 {
		s = s[:100]
	}
	return strings.TrimSpace(s)
}
