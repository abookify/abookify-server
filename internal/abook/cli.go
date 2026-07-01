package abook

import (
	"archive/zip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// This file backs the standalone `abook` CLI (cmd/abook) and any other consumer
// that wants to inspect/extract/pack a .abook without a running server. A
// .abook is a standard deflate ZIP: manifest.json + a per-work book.db carved
// from the monolith + optional cover + optional bundled audio.

// FileEntry describes one member of the .abook ZIP.
type FileEntry struct {
	Name           string `json:"name"`
	Size           int64  `json:"size"`            // uncompressed bytes
	CompressedSize int64  `json:"compressed_size"` // stored bytes
	Method         string `json:"method"`          // "deflate" | "store" | other
}

// SourceRow summarizes one book/source row from book.db (for `abook info`).
type SourceRow struct {
	Filename   string `json:"filename"`
	Format     string `json:"format"`
	MediaType  string `json:"media_type"`  // "audio" | "text"
	Origin     string `json:"origin"`      // publisher_epub, whisper_transcript, …
	Visibility string `json:"visibility"`  // visible | internal
	Chapters   int    `json:"chapters"`
	Bundled    bool   `json:"bundled"`     // audio bundled in this .abook
}

// ArchiveInfo is the full read-only view `abook info` reports.
type ArchiveInfo struct {
	Manifest       *Manifest   `json:"manifest"`
	Files          []FileEntry `json:"files"`
	Sources        []SourceRow `json:"sources"`
	Chunks         int         `json:"chunks"`
	EmbeddedChunks int         `json:"embedded_chunks"`
	Paragraphs     int         `json:"paragraphs"`
	// StandardDeflate is true when every entry uses STORE or DEFLATE — i.e. a
	// browser (DecompressionStream) or any stock ZIP tool can read it.
	StandardDeflate bool `json:"standard_deflate"`
}

// Inspect reads a .abook and returns its manifest, file list, and a book.db
// source/chunk summary. It does not extract to disk (book.db is copied to a
// temp file only to be queried, then removed).
func Inspect(abookPath string) (*ArchiveInfo, error) {
	zr, err := zip.OpenReader(abookPath)
	if err != nil {
		return nil, fmt.Errorf("open .abook: %w", err)
	}
	defer zr.Close()

	info := &ArchiveInfo{StandardDeflate: true}
	var bookDBFile *zip.File
	for _, f := range zr.File {
		method := "other"
		switch f.Method {
		case zip.Store:
			method = "store"
		case zip.Deflate:
			method = "deflate"
		default:
			info.StandardDeflate = false
		}
		info.Files = append(info.Files, FileEntry{
			Name: f.Name, Size: int64(f.UncompressedSize64),
			CompressedSize: int64(f.CompressedSize64), Method: method,
		})
		if f.Name == "manifest.json" {
			data, err := readZipFile(f)
			if err == nil {
				var m Manifest
				if json.Unmarshal(data, &m) == nil {
					info.Manifest = &m
				}
			}
		}
		if f.Name == "book.db" {
			bookDBFile = f
		}
	}

	if bookDBFile != nil {
		if err := summarizeBookDB(bookDBFile, info); err != nil {
			return nil, fmt.Errorf("read book.db: %w", err)
		}
	}
	return info, nil
}

// summarizeBookDB copies book.db to a temp file, opens it, and fills the source
// / chunk / paragraph counts.
func summarizeBookDB(f *zip.File, info *ArchiveInfo) error {
	tmp, err := os.CreateTemp("", "abook-inspect-*.db")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	rc, err := f.Open()
	if err != nil {
		tmp.Close()
		return err
	}
	_, cpErr := io.Copy(tmp, rc)
	rc.Close()
	tmp.Close()
	if cpErr != nil {
		return cpErr
	}

	dbc, err := sql.Open("sqlite", tmp.Name())
	if err != nil {
		return err
	}
	defer dbc.Close()

	rows, err := dbc.Query(`SELECT filename, format, media_type, origin, visibility, chapter_count, asset_path IS NOT NULL FROM books ORDER BY media_type, filename`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s SourceRow
		if err := rows.Scan(&s.Filename, &s.Format, &s.MediaType, &s.Origin, &s.Visibility, &s.Chapters, &s.Bundled); err != nil {
			rows.Close()
			return err
		}
		info.Sources = append(info.Sources, s)
	}
	rows.Close()

	dbc.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&info.Chunks)
	dbc.QueryRow(`SELECT COUNT(*) FROM chunks WHERE embedding IS NOT NULL AND LENGTH(embedding) > 0`).Scan(&info.EmbeddedChunks)
	dbc.QueryRow(`SELECT COUNT(*) FROM paragraphs`).Scan(&info.Paragraphs)
	return nil
}

// ExtractTo unzips a .abook into outDir (created if absent) and returns the
// list of written paths. Guards against zip-slip (entries escaping outDir).
func ExtractTo(abookPath, outDir string) ([]string, error) {
	zr, err := zip.OpenReader(abookPath)
	if err != nil {
		return nil, fmt.Errorf("open .abook: %w", err)
	}
	defer zr.Close()

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return nil, err
	}

	var written []string
	for _, f := range zr.File {
		dest := filepath.Join(absOut, f.Name)
		// zip-slip guard: the resolved path must stay under outDir.
		if !strings.HasPrefix(dest, absOut+string(os.PathSeparator)) && dest != absOut {
			return written, fmt.Errorf("unsafe zip entry %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return written, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return written, err
		}
		if err := writeZipEntry(f, dest); err != nil {
			return written, err
		}
		written = append(written, dest)
	}
	return written, nil
}

// PackDir builds a .abook (standard deflate ZIP) from a directory that holds an
// unpacked archive (manifest.json + book.db, plus optional cover/audio). It
// recomputes book.db's sha256 in the manifest so an edited book.db stays
// consistent. Every entry is written with DEFLATE (mimetype-style STORE isn't
// used by this format) so the result is browser-decodable.
func PackDir(srcDir, outPath string) error {
	manPath := filepath.Join(srcDir, "manifest.json")
	dbPath := filepath.Join(srcDir, "book.db")
	if _, err := os.Stat(manPath); err != nil {
		return fmt.Errorf("no manifest.json in %s", srcDir)
	}
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no book.db in %s", srcDir)
	}

	// Update the manifest's book.db checksum to match the on-disk book.db.
	manData, err := os.ReadFile(manPath)
	if err != nil {
		return err
	}
	var m Manifest
	if err := json.Unmarshal(manData, &m); err != nil {
		return fmt.Errorf("parse manifest.json: %w", err)
	}
	dbBytes, err := os.ReadFile(dbPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(dbBytes)
	if m.Checksums == nil {
		m.Checksums = map[string]string{}
	}
	m.Checksums["book.db"] = "sha256:" + hex.EncodeToString(sum[:])
	updatedManifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)

	absSrc, _ := filepath.Abs(srcDir)
	err = filepath.Walk(srcDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		abs, _ := filepath.Abs(path)
		rel, _ := filepath.Rel(absSrc, abs)
		rel = filepath.ToSlash(rel)
		w, err := zw.Create(rel) // zw.Create defaults to DEFLATE
		if err != nil {
			return err
		}
		if rel == "manifest.json" {
			_, err = w.Write(updatedManifest) // write the checksum-updated manifest
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
	if err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func writeZipEntry(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}
