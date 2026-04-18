package abook

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pj/abookify/internal/db"
)

// Export creates an .abook file from a work.
func Export(store *db.Store, work *db.Work, outputPath string) error {
	return ExportWithDirs(store, work, outputPath, "")
}

// ExportWithDirs is Export with an explicit libraryDir so covers can be
// bundled from {libraryDir}/covers/work-{id}.jpg. If libraryDir is empty,
// falls back to common defaults.
func ExportWithDirs(store *db.Store, work *db.Work, outputPath, libraryDir string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	manifest := Manifest{
		Format:      "abook",
		Version:     2,
		Title:       work.Title,
		Author:      work.Author,
		Series:      work.Series,
		SeriesIndex: work.SeriesIndex,
		Language:    "en",
		Created:     time.Now().UTC().Format(time.RFC3339),
		Generator:   "abookify v0.2.0",
	}

	// Include cover art if present. Covers live at {libraryDir}/covers/work-{id}.jpg.
	coverBases := []string{}
	if libraryDir != "" {
		coverBases = append(coverBases, libraryDir+"/covers")
	}
	coverBases = append(coverBases, "/library/covers", "./library/covers")
	for _, base := range coverBases {
		coverPath := fmt.Sprintf("%s/work-%d.jpg", base, work.ID)
		if data, err := os.ReadFile(coverPath); err == nil && len(data) > 0 {
			if err := writeToZip(w, "cover.jpg", data); err == nil {
				manifest.Cover = "cover.jpg"
			}
			break
		}
	}

	// Bookmarks for this work.
	if bookmarks, err := store.ListBookmarks(work.ID); err == nil && len(bookmarks) > 0 {
		if data, err := json.MarshalIndent(bookmarks, "", "  "); err == nil {
			if err := writeToZip(w, "bookmarks.json", data); err == nil {
				manifest.Bookmarks = "bookmarks.json"
			}
		}
	}

	// Alignments (forced alignment pairs) for this work.
	if alignments, err := store.ListAlignmentsForWork(work.ID); err == nil {
		for i, a := range alignments {
			path := fmt.Sprintf("alignments/%d-%d.json", a.FromBookID, a.ToBookID)
			data, err := json.MarshalIndent(a, "", "  ")
			if err != nil {
				continue
			}
			if err := writeToZip(w, path, data); err == nil {
				manifest.Alignments = append(manifest.Alignments, path)
			}
			_ = i
		}
	}

	// Determine chapter structure.
	// We use text chapters as the primary structure if available,
	// and map audio files to them. If no text, use audio files directly.
	if work.HasText && len(work.TextFiles) > 0 {
		textBook := work.TextFiles[0]
		chapters, err := store.ListChapters(textBook.ID)
		if err != nil {
			return fmt.Errorf("list chapters: %w", err)
		}

		// Build audio file lookup by chapter link
		links, _ := store.GetChapterLinks(work.ID)
		audioByTextIdx := map[int]*db.Book{}
		for _, link := range links {
			for i := range work.AudioFiles {
				if work.AudioFiles[i].ID == link.AudioBookID {
					audioByTextIdx[link.TextIndex] = &work.AudioFiles[i]
					break
				}
			}
		}

		for _, ch := range chapters {
			// Skip very short chapters (TOC, license, etc.)
			if ch.WordCount < 20 {
				continue
			}

			chNum := fmt.Sprintf("%03d", ch.Index+1)
			entry := Chapter{
				Index:     ch.Index,
				Title:     ch.Title,
				WordCount: ch.WordCount,
			}

			// Add text
			fullCh, err := store.GetChapterContent(textBook.ID, ch.Index)
			if err == nil && fullCh != nil && fullCh.Content != "" {
				textPath := fmt.Sprintf("text/chapter-%s.html", chNum)
				html := wrapHTML(ch.Title, fullCh.Content)
				if err := writeToZip(w, textPath, []byte(html)); err != nil {
					return err
				}
				entry.Text = textPath
			}

			// Add linked audio if available
			if af, ok := audioByTextIdx[ch.Index]; ok {
				ext := filepath.Ext(af.Filename)
				audioPath := fmt.Sprintf("audio/chapter-%s%s", chNum, ext)
				if err := copyFileToZip(w, audioPath, af.Path); err != nil {
					// Log but don't fail — audio might be missing
					continue
				}
				entry.Audio = audioPath

				// Sync data (word timestamps) if present for karaoke offline.
				if sync, err := store.GetSyncData(work.ID, af.ID, ch.Index); err == nil && sync != "" {
					syncPath := fmt.Sprintf("sync/chapter-%s.json", chNum)
					if err := writeToZip(w, syncPath, []byte(sync)); err == nil {
						entry.Sync = syncPath
					}
				}
			}

			manifest.Chapters = append(manifest.Chapters, entry)
		}
	} else if work.HasAudio {
		// Audio-only work: each audio file is a chapter
		for i, af := range work.AudioFiles {
			chNum := fmt.Sprintf("%03d", i+1)
			ext := filepath.Ext(af.Filename)
			audioPath := fmt.Sprintf("audio/chapter-%s%s", chNum, ext)

			if err := copyFileToZip(w, audioPath, af.Path); err != nil {
				continue
			}

			title := af.Title
			if title == "" {
				title = af.Filename
			}

			entry := Chapter{
				Index: i,
				Title: title,
				Audio: audioPath,
			}

			// Sync data if present (from Whisper transcription).
			if sync, err := store.GetSyncData(work.ID, af.ID, i); err == nil && sync != "" {
				syncPath := fmt.Sprintf("sync/chapter-%s.json", chNum)
				if err := writeToZip(w, syncPath, []byte(sync)); err == nil {
					entry.Sync = syncPath
				}
			}

			manifest.Chapters = append(manifest.Chapters, entry)
		}
	}

	// Write manifest
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := writeToZip(w, "manifest.json", manifestJSON); err != nil {
		return err
	}

	return nil
}

func wrapHTML(title, plainText string) string {
	// Convert plain text to simple HTML paragraphs
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"UTF-8\"></head><body>\n")
	sb.WriteString(fmt.Sprintf("<h1>%s</h1>\n", htmlEscape(title)))

	for _, para := range strings.Split(plainText, "\n") {
		para = strings.TrimSpace(para)
		if para != "" {
			sb.WriteString(fmt.Sprintf("<p>%s</p>\n", htmlEscape(para)))
		}
	}

	sb.WriteString("</body></html>\n")
	return sb.String()
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func writeToZip(w *zip.Writer, name string, data []byte) error {
	f, err := w.Create(name)
	if err != nil {
		return fmt.Errorf("create zip entry %s: %w", name, err)
	}
	_, err = f.Write(data)
	return err
}

func copyFileToZip(w *zip.Writer, name string, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer src.Close()

	f, err := w.Create(name)
	if err != nil {
		return fmt.Errorf("create zip entry %s: %w", name, err)
	}

	_, err = io.Copy(f, src)
	return err
}
