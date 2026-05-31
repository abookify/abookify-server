package abook

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pj/abookify/internal/db"
)

const (
	generator = "abookify v0.3.0"
	language  = "en"
)

// ExportOptions controls what a v2 .abook carries.
type ExportOptions struct {
	// IncludeAudio bundles the work's audio files under audio/. When false the
	// container holds only book.db + manifest + cover (audio streams from the
	// server). book.db books.asset_path is set only for bundled audio.
	IncludeAudio bool
	// IncludeEmbeddings carries chunk embedding blobs in book.db (larger file,
	// enables future on-device vector search). Omitted by default.
	IncludeEmbeddings bool
}

// Export creates a v2 .abook for a work (audio bundled, embeddings omitted).
func Export(store *db.Store, work *db.Work, outputPath string) error {
	return ExportWithDirs(store, work, outputPath, "")
}

// ExportWithDirs is Export with an explicit libraryDir so the cover and audio
// files can be located. Audio is bundled; embeddings are omitted. This is the
// shape the web "download .abook" button produces.
func ExportWithDirs(store *db.Store, work *db.Work, outputPath, libraryDir string) error {
	return ExportV2(store, work, outputPath, libraryDir, ExportOptions{IncludeAudio: true})
}

// ExportV2 writes a v2 .abook container: manifest.json + a per-work book.db
// carved from the monolith + cover, plus bundled audio when opts.IncludeAudio.
func ExportV2(store *db.Store, work *db.Work, outputPath, libraryDir string, opts ExportOptions) error {
	sum := SummarizeWork(store, work)

	// Map audio books that have a real on-disk file to an in-zip asset path.
	assetPaths := map[int64]string{}
	type audioAsset struct{ srcPath, zipPath string }
	var audioAssets []audioAsset
	if opts.IncludeAudio {
		for _, bk := range work.AudioFiles {
			if bk.Path == "" {
				continue
			}
			if _, err := os.Stat(bk.Path); err != nil {
				continue // generated:// or missing file — skip bundling
			}
			ext := filepath.Ext(bk.Filename)
			if ext == "" {
				ext = filepath.Ext(bk.Path)
			}
			zipPath := fmt.Sprintf("audio/book-%d%s", bk.ID, ext)
			assetPaths[bk.ID] = zipPath
			audioAssets = append(audioAssets, audioAsset{srcPath: bk.Path, zipPath: zipPath})
		}
	}

	// Build book.db in a temp file, then stream it into the zip.
	tmpDB, err := os.CreateTemp("", "abook-book-*.db")
	if err != nil {
		return fmt.Errorf("temp book.db: %w", err)
	}
	tmpPath := tmpDB.Name()
	tmpDB.Close()
	defer os.Remove(tmpPath)

	if err := buildBookDB(store, work, sum, tmpPath, assetPaths, opts.IncludeEmbeddings); err != nil {
		return fmt.Errorf("build book.db: %w", err)
	}

	dbBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return fmt.Errorf("read book.db: %w", err)
	}
	sumHash := sha256.Sum256(dbBytes)

	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	defer w.Close()

	if err := writeToZip(w, "book.db", dbBytes); err != nil {
		return err
	}

	manifest := Manifest{
		Format:         "abook",
		Version:        2,
		WorkID:         work.ID,
		Title:          work.Title,
		Author:         work.Author,
		Language:       language,
		SourceKind:     sum.SourceKind,
		SchemaVersion:  BookDBSchemaVersion,
		ContentVersion: work.ContentVersion,
		Generator:      generator,
		CoveragePct:    sum.CoveragePct,
		AlignMethod:    sum.AlignMethod,
		AlignUnit:      sum.AlignUnit,
		Assets:         Assets{DB: "book.db", AudioDir: "audio/"},
		Checksums:      map[string]string{"book.db": "sha256:" + hex.EncodeToString(sumHash[:])},
	}

	// Cover. Covers live at {libraryDir}/covers/work-{id}.jpg.
	coverBases := []string{}
	if libraryDir != "" {
		coverBases = append(coverBases, filepath.Join(libraryDir, "covers"))
	}
	coverBases = append(coverBases, "/library/covers", "./library/covers")
	for _, base := range coverBases {
		coverPath := filepath.Join(base, fmt.Sprintf("work-%d.jpg", work.ID))
		if data, err := os.ReadFile(coverPath); err == nil && len(data) > 0 {
			if err := writeToZip(w, "cover.jpg", data); err == nil {
				manifest.Assets.Cover = "cover.jpg"
			}
			break
		}
	}

	// Audio files.
	for _, a := range audioAssets {
		if err := copyFileToZip(w, a.zipPath, a.srcPath); err != nil {
			return fmt.Errorf("bundle audio %s: %w", a.zipPath, err)
		}
	}

	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := writeToZip(w, "manifest.json", manifestJSON); err != nil {
		return err
	}

	return nil
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
