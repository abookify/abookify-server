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
	"sort"
	"strings"
	"time"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
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
	// OnlyBookIDs restricts the container to these books (nil/empty = whole
	// work). A work in a personal library legitimately accumulates several
	// narrations, transcripts and editions — a DISTRIBUTED .abook must not:
	// the clean-Carol requirement is exactly one narration and one text, and
	// carving the whole of work 85 dragged five LibriVox recordings and a
	// transcript along. Alignments, sync rows and chapter links referencing
	// excluded books are dropped with them, never left dangling.
	OnlyBookIDs map[int64]bool
	// AllowIncoherent bypasses the coherence gate. Off by default: a .abook that
	// carries an edition-label split (one narration presented as multiple editions)
	// must not be produced — the showcase featured artifact once shipped exactly that
	// to strangers. The escape hatch exists only for deliberate diagnostics.
	AllowIncoherent bool
	// Public marks a distribution export. Every bundled book must carry a
	// source_provenance row with cleared=true (+ cleared_by); the cover is
	// bundled only if its recorded source is a cleared bundled book's own
	// embedded art, or the work's cover has its own cleared row. Otherwise the
	// export is REFUSED (ErrNotCleared, naming what is missing) — never
	// silently degraded, so a human can't publish an uncleared file by
	// forgetting a step. The manifest then carries a Publishing block.
	Public bool
}

// ErrNotCleared is returned by a Public export that cannot prove clearance.
type ErrNotCleared struct {
	Missing []string
}

func (e *ErrNotCleared) Error() string {
	return "public export refused — not cleared for redistribution: " + strings.Join(e.Missing, "; ")
}

// bundledBooks lists the books this export will carry (OnlyBookIDs honoured).
func bundledBooks(work *db.Work, opts ExportOptions) []db.Book {
	var out []db.Book
	for _, b := range work.AudioFiles {
		if len(opts.OnlyBookIDs) == 0 || opts.OnlyBookIDs[b.ID] {
			out = append(out, b)
		}
	}
	for _, b := range work.TextFiles {
		if len(opts.OnlyBookIDs) == 0 || opts.OnlyBookIDs[b.ID] {
			out = append(out, b)
		}
	}
	return out
}

// verifyPublishing builds the Publishing block for a public export or returns
// ErrNotCleared. It reads only human-authored rows + the cover's own sidecar.
func verifyPublishing(store *db.Store, work *db.Work, libraryDir string, opts ExportOptions) (*Publishing, error) {
	books := bundledBooks(work, opts)
	ids := make([]int64, 0, len(books))
	for _, b := range books {
		ids = append(ids, b.ID)
	}
	rows, err := store.GetSourceProvenance("book", ids)
	if err != nil {
		return nil, err
	}
	pub := &Publishing{Public: true, VerifiedAt: time.Now().UTC().Format(time.RFC3339)}
	var missing []string
	clearedBook := map[int64]bool{}
	for _, b := range books {
		r, ok := rows[b.ID]
		ps := PublishedSource{BookID: b.ID, Media: b.MediaType, Format: b.Format, Origin: b.Origin,
			Kind: r.Kind, SourceURL: r.SourceURL, License: r.License, Cleared: r.Cleared, ClearedBy: r.ClearedBy, Note: r.Note}
		switch {
		case !ok:
			missing = append(missing, fmt.Sprintf("book %d (%s %s %q) has no provenance declaration", b.ID, b.MediaType, b.Format, b.Filename))
		case !r.Cleared:
			missing = append(missing, fmt.Sprintf("book %d (%s %s %q) is declared (%s) but NOT cleared", b.ID, b.MediaType, b.Format, b.Filename, r.Kind))
		default:
			clearedBook[b.ID] = true
		}
		pub.Sources = append(pub.Sources, ps)
	}
	// Cover: what did the file's own sidecar say, and is that source cleared?
	coverPath := ""
	if libraryDir != "" {
		coverPath = filepath.Join(libraryDir, "covers", fmt.Sprintf("work-%d.jpg", work.ID))
	}
	pc := PublishedCover{}
	if coverPath != "" {
		if _, err := os.Stat(coverPath); err == nil {
			src, hasSidecar := library.ReadCoverSource(coverPath)
			coverRows, _ := store.GetSourceProvenance("cover", []int64{work.ID})
			cr, hasRow := coverRows[work.ID]
			switch {
			case hasRow && cr.Cleared:
				pc = PublishedCover{Bundled: true, Source: src.Source, Ref: src.Ref, Cleared: true, ClearedBy: cr.ClearedBy, Reason: "cover has its own clearance: " + cr.Note}
			case hasSidecar && (src.Source == "epub" || src.Source == "audio") && clearedBook[src.BookID]:
				pc = PublishedCover{Bundled: true, Source: src.Source, Ref: src.Ref, Cleared: true, ClearedBy: rows[src.BookID].ClearedBy, Reason: "embedded art of cleared bundled book " + fmt.Sprint(src.BookID)}
			case hasSidecar:
				pc = PublishedCover{Bundled: false, Source: src.Source, Ref: src.Ref, Reason: "cover source " + src.Source + " is not a cleared bundled book and has no clearance of its own — omitted"}
			default:
				pc = PublishedCover{Bundled: false, Source: "unknown", Reason: "cover has no recorded source (predates provenance tracking) — omitted"}
			}
		} else {
			pc = PublishedCover{Bundled: false, Reason: "work has no cover file"}
		}
	}
	pub.Cover = pc
	if len(missing) > 0 {
		return nil, &ErrNotCleared{Missing: missing}
	}
	return pub, nil
}

// abookCoherenceProblems checks the distribution invariant INDEPENDENTLY of canon
// (which groups by audio directory — meaningless once export flattens files into
// audio/): a single narration, keyed by (origin, voice/album), must carry ONE edition
// label, and a label must not span two narrations. A split is the two-editions-of-three
// defect. Empty result = coherent.
func abookCoherenceProblems(work *db.Work) []string {
	type key struct{ origin, album string }
	labels := map[key]map[string]bool{}
	for i := range work.AudioFiles {
		b := &work.AudioFiles[i]
		k := key{b.Origin, b.Album}
		if labels[k] == nil {
			labels[k] = map[string]bool{}
		}
		labels[k][b.Edition] = true
	}
	var probs []string
	for k, ls := range labels {
		if len(ls) > 1 {
			var lst []string
			for l := range ls {
				lst = append(lst, fmt.Sprintf("%q", l))
			}
			sort.Strings(lst)
			probs = append(probs, fmt.Sprintf("edition-label split: narration (%s, voice=%q) carries %d labels [%s]",
				k.origin, k.album, len(ls), strings.Join(lst, ", ")))
		}
	}
	labelNarr := map[string]map[key]bool{}
	for k, ls := range labels {
		for l := range ls {
			if l == "" {
				continue
			}
			if labelNarr[l] == nil {
				labelNarr[l] = map[key]bool{}
			}
			labelNarr[l][k] = true
		}
	}
	for l, narrs := range labelNarr {
		if len(narrs) > 1 {
			probs = append(probs, fmt.Sprintf("label %q spans %d narrations", l, len(narrs)))
		}
	}
	sort.Strings(probs)
	return probs
}

// isOriginalEbookFormat reports whether a text book is an ORIGINAL user-supplied
// ebook worth bundling (vs a derived transcript / tts-preprocessed text, which
// live only in book.db).
func isOriginalEbookFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "epub", "mobi", "azw3", "azw", "txt", "pdf":
		return true
	}
	return false
}

// Export creates a v2 .abook for a work (audio bundled, embeddings omitted).
func Export(store *db.Store, work *db.Work, outputPath string) error {
	return ExportWithDirs(store, work, outputPath, "")
}

// ExportWithDirs is Export with an explicit libraryDir so the cover and audio
// files can be located. Audio + chunk embeddings are both bundled, so the
// container is a self-contained offline copy that supports on-device semantic
// search. This is the shape the web "download .abook" button produces.
func ExportWithDirs(store *db.Store, work *db.Work, outputPath, libraryDir string) error {
	return ExportV2(store, work, outputPath, libraryDir, ExportOptions{IncludeAudio: true, IncludeEmbeddings: true})
}

// ExportV2 writes a v2 .abook container: manifest.json + a per-work book.db
// carved from the monolith + cover, plus bundled audio when opts.IncludeAudio.
func ExportV2(store *db.Store, work *db.Work, outputPath, libraryDir string, opts ExportOptions) error {
	// PUBLIC export: prove clearance BEFORE creating anything, so a refused
	// export leaves no partial file behind (the verdict is reused below).
	var publishing *Publishing
	if opts.Public {
		pubv, perr := verifyPublishing(store, work, libraryDir, opts)
		if perr != nil {
			return perr
		}
		publishing = pubv
	}
	if len(opts.OnlyBookIDs) > 0 {
		work = filterWorkBooks(work, opts.OnlyBookIDs)
	}
	// COHERENCE GATE: refuse to produce a .abook that carries an edition-label split.
	// Runs AFTER the OnlyBookIDs filter — a deliberate single-narration carve is
	// coherent even when the full library work is not. Today's clean sweep is a
	// snapshot; this makes it a property that holds without anyone remembering to look.
	if !opts.AllowIncoherent {
		if probs := abookCoherenceProblems(work); len(probs) > 0 {
			return fmt.Errorf("refusing to export an incoherent work (would ship a split): %s", strings.Join(probs, "; "))
		}
	}
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

	// Bundle the ORIGINAL ebook source file(s) under originals/ by default —
	// they're small and give fidelity/portability beyond the carved book.db
	// (which stays the render source). Only real on-disk, visible, original
	// ebook formats (not derived transcripts / tts-preprocessed text).
	type origAsset struct{ srcPath, zipPath string }
	var origAssets []origAsset
	var origManifest []OriginalFile
	usedOrig := map[string]bool{}
	for i := range work.TextFiles {
		bk := &work.TextFiles[i]
		if bk.Path == "" || bk.Visibility == "internal" || !isOriginalEbookFormat(bk.Format) {
			continue
		}
		if _, err := os.Stat(bk.Path); err != nil {
			continue // generated / missing — nothing to bundle
		}
		base := sanitizeFilename(filepath.Base(bk.Filename))
		if base == "" {
			base = fmt.Sprintf("book-%d.%s", bk.ID, bk.Format)
		}
		zipPath := "originals/" + base
		for n := 2; usedOrig[zipPath]; n++ { // de-dup identical filenames
			zipPath = fmt.Sprintf("originals/book-%d-%s", bk.ID, base)
			if !usedOrig[zipPath] {
				break
			}
			_ = n
		}
		usedOrig[zipPath] = true
		origAssets = append(origAssets, origAsset{srcPath: bk.Path, zipPath: zipPath})
		origManifest = append(origManifest, OriginalFile{
			Path: zipPath, Filename: bk.Filename, Format: bk.Format, Origin: bk.Origin,
		})
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

	// Identify the embedding model/dim from the carved vectors so a consumer
	// embeds queries with a matching model (cross-model cosine is meaningless).
	embedDim, embedModel := 0, ""
	if opts.IncludeEmbeddings {
		embedDim = embeddingDimOf(tmpPath)
		embedModel = embedModelForDim(embedDim)
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
		Format:           "abook",
		Version:          2,
		MinorVersion:     1, // originals/ bundling
		WorkID:           work.ID,
		Title:            work.Title,
		Author:           work.Author,
		Language:         language,
		SourceKind:       sum.SourceKind,
		SchemaVersion:    BookDBSchemaVersion,
		ContentVersion:   work.ContentVersion,
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Provenance:       sum.Provenance,
		Generator:        generator,
		CoveragePct:      sum.CoveragePct,
		AlignMethod:      sum.AlignMethod,
		AlignUnit:        sum.AlignUnit,
		Assets:           Assets{DB: "book.db", AudioDir: "audio/", OriginalsDir: "originals/"},
		HasEmbeddings:    opts.IncludeEmbeddings && embedDim > 0,
		EmbeddingModel:   embedModel,
		EmbeddingDim:     embedDim,
		HasAudio:         len(audioAssets) > 0,
		HasOriginalEbook: len(origManifest) > 0,
		Originals:        origManifest,
		Checksums:        map[string]string{"book.db": "sha256:" + hex.EncodeToString(sumHash[:])},
	}

	if publishing != nil {
		manifest.Publishing = publishing
	}
	// Cover. Covers live at {libraryDir}/covers/work-{id}.jpg.
	coverBases := []string{}
	if publishing != nil && !publishing.Cover.Bundled {
		coverBases = nil // decided above: not ours to publish
	} else if libraryDir != "" {
		coverBases = append(coverBases, filepath.Join(libraryDir, "covers"))
	}
	if publishing == nil {
		coverBases = append(coverBases, "/library/covers", "./library/covers")
	}
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

	// Original ebook source files.
	for _, a := range origAssets {
		if err := copyFileToZip(w, a.zipPath, a.srcPath); err != nil {
			return fmt.Errorf("bundle original %s: %w", a.zipPath, err)
		}
	}

	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	// A PUBLIC export is self-contained: ATTRIBUTION.txt is generated from the
	// verified publishing block (never hand-written into the zip afterwards).
	if publishing != nil {
		if err := writeToZip(w, "ATTRIBUTION.txt", []byte(attributionText(work, publishing))); err != nil {
			return err
		}
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

// filterWorkBooks returns a shallow copy of the work holding only the selected
// books, with the derived fields (HasAudio/HasText/DisplayTextBookID) made
// consistent with the reduced set.
func filterWorkBooks(work *db.Work, only map[int64]bool) *db.Work {
	w := *work
	w.AudioFiles = nil
	w.TextFiles = nil
	for _, b := range work.AudioFiles {
		if only[b.ID] {
			w.AudioFiles = append(w.AudioFiles, b)
		}
	}
	for _, b := range work.TextFiles {
		if only[b.ID] {
			w.TextFiles = append(w.TextFiles, b)
		}
	}
	w.HasAudio = len(w.AudioFiles) > 0
	w.HasText = len(w.TextFiles) > 0
	if !only[w.DisplayTextBookID] {
		w.DisplayTextBookID = 0
		for _, b := range w.TextFiles {
			w.DisplayTextBookID = b.ID
			break
		}
	}
	return &w
}

// DryRunPublic answers "would a public export of this work succeed right now,
// and what would it say about the cover?" without writing anything.
func DryRunPublic(store *db.Store, work *db.Work, libraryDir string) (*Publishing, error) {
	return verifyPublishing(store, work, libraryDir, ExportOptions{Public: true})
}

// attributionText renders the human-readable attribution for a public export
// from the verified sources — the same facts the manifest's publishing block
// carries, in prose a downloader can read.
func attributionText(work *db.Work, pub *Publishing) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ATTRIBUTION — %s\n", work.Title)
	fmt.Fprintf(&b, "%s\n\n", strings.Repeat("=", len("ATTRIBUTION — ")+len(work.Title)))
	fmt.Fprintf(&b, "WORK\n  Title:   %s\n  Author:  %s\n\n", work.Title, work.Author)
	fmt.Fprintf(&b, "SOURCES (each verified and cleared for public redistribution by the person named)\n")
	for _, s := range pub.Sources {
		fmt.Fprintf(&b, "  - %s %s (book %d): kind=%s", s.Media, s.Format, s.BookID, s.Kind)
		if s.SourceURL != "" {
			fmt.Fprintf(&b, "  source=%s", s.SourceURL)
		}
		if s.License != "" {
			fmt.Fprintf(&b, "  license=%s", s.License)
		}
		fmt.Fprintf(&b, "  cleared_by=%s", s.ClearedBy)
		if s.Note != "" {
			fmt.Fprintf(&b, "\n      note: %s", s.Note)
		}
		b.WriteString("\n")
	}
	if pub.Cover.Bundled {
		fmt.Fprintf(&b, "\nCOVER\n  %s (%s) — cleared_by=%s\n", pub.Cover.Source, pub.Cover.Ref, pub.Cover.ClearedBy)
	} else {
		fmt.Fprintf(&b, "\nCOVER\n  none bundled — %s\n", pub.Cover.Reason)
	}
	fmt.Fprintf(&b, "\nVerified %s by Abookify's public-export gate (manifest.json → publishing).\n", pub.VerifiedAt)
	return b.String()
}
