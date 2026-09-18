package abook

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"database/sql"

	"github.com/pj/abookify/internal/db"
)

// Import reads a v2 .abook file and ingests it into the library as a NEW work.
// The bundled book.db is the source; its rows are copied into the monolith
// with fresh server-assigned IDs (only book ids need remapping — chapter and
// sync references key off index numbers, which are stable). Audio + cover are
// extracted under {libraryDir}/abooks/{title}/.
// ImportOptions steers an import that must land INSIDE an existing work.
//
// THE DEFECT THIS REMOVES (2026-09-18, launch-relevant): the live showcase picker
// offers both the human-narrated and the AI-narrated A Christmas Carol. The first
// import made a work; the second, same title + author, hit identity dedupe and was
// SKIPPED — the picker said "Already in your library" and opened the narration the
// visitor already had. The second narration never arrived and the button said it
// did. Even forced in (on_conflict=new) it became a duplicate WORK, and both
// narrations extracted into the same abooks/<title>/audio/ directory — canon groups
// editions BY DIRECTORY, so that is one edition with two voices: incoherent (the
// fleet's 8197 shape). Two problems, one root: the importer colocated editions.
//
// Now every import extracts into abooks/<title>/<edition>/ (one directory per
// narration, so editions stay distinct by construction), and IntoWorkID makes a
// same-title file land as a SECOND EDITION of the existing work: its narration is
// added, a text source identical to one the work already holds (same format,
// chapter count and word count — the same publisher EPUB in both files) is reused
// rather than duplicated, and its alignments/links are remapped onto the reused
// text. On any failure only the books added by THIS import are rolled back — the
// existing work is never touched. Existing works keep their paths: nothing here
// moves a file that is already in the library, so PJ's two-edition works need no
// migration.
type ImportOptions struct {
	IntoWorkID int64 // 0 = create a new work (the classic import)
}

// ImportResult says what the import did, for the caller's UI.
type ImportResult struct {
	WorkID      int64
	Edition     string // the edition directory name this file landed in
	AddedBooks  int
	ReusedTexts int  // incoming text sources that matched an existing one and were reused
	Skipped     bool // IntoWorkID already held this exact edition (idempotent re-import)
}

// Import is the classic entry point: a new work.
func Import(store *db.Store, abookPath string, libraryDir string) error {
	_, err := ImportInto(store, abookPath, libraryDir, ImportOptions{})
	return err
}

// ImportInto imports an .abook, optionally as an additional edition of an
// existing work (see ImportOptions).
func ImportInto(store *db.Store, abookPath string, libraryDir string, opts ImportOptions) (*ImportResult, error) {
	r, err := zip.OpenReader(abookPath)
	if err != nil {
		return nil, fmt.Errorf("open abook: %w", err)
	}
	defer r.Close()

	manifestData, err := readFromZip(&r.Reader, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Format != "abook" {
		return nil, fmt.Errorf("not an abook file (format: %q)", manifest.Format)
	}
	if manifest.Version != 2 {
		return nil, fmt.Errorf("unsupported .abook version %d (expected 2)", manifest.Version)
	}

	log.Printf("abook import: %q by %s (v2)", manifest.Title, manifest.Author)

	safeName := sanitizeFilename(manifest.Title)
	// One directory PER EDITION under the title: canon derives editions from the
	// audio files' directory, so two narrations of one book must never share one.
	edition, err := editionDirName(&r.Reader, manifest.ContentVersion)
	if err != nil {
		return nil, err
	}
	outDir := filepath.Join(libraryDir, "abooks", safeName, edition)
	if opts.IntoWorkID != 0 {
		// Idempotent: the work already holds this edition (same narration re-tapped
		// in the picker) → nothing to add, say so.
		if w, err := store.GetWork(opts.IntoWorkID); err == nil && w != nil {
			for _, b := range w.AudioFiles {
				if filepath.Dir(b.Path) == filepath.Join(outDir, "audio") {
					return &ImportResult{WorkID: opts.IntoWorkID, Edition: edition, Skipped: true}, nil
				}
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(outDir, "audio"), 0755); err != nil {
		return nil, fmt.Errorf("create out dir: %w", err)
	}

	// Extract book.db + audio + cover.
	for _, f := range r.File {
		if f.FileInfo().IsDir() || f.Name == "manifest.json" {
			continue
		}
		destPath := filepath.Join(outDir, f.Name)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		out, err := os.Create(destPath)
		if err != nil {
			rc.Close()
			os.RemoveAll(outDir)
			return nil, fmt.Errorf("create %q: %w", f.Name, err)
		}
		// A silently-truncated write (disk full) would leave a book with broken
		// audio that still imports "successfully" — check it and fail loudly.
		if _, cerr := io.Copy(out, rc); cerr != nil {
			out.Close()
			rc.Close()
			os.RemoveAll(outDir)
			return nil, fmt.Errorf("extract %q failed (out of disk space?): %w", f.Name, cerr)
		}
		out.Close()
		rc.Close()
	}

	dbPath := filepath.Join(outDir, manifest.Assets.DB)
	if want := manifest.Checksums["book.db"]; want != "" {
		if err := verifyChecksum(dbPath, want); err != nil {
			os.RemoveAll(outDir)
			return nil, fmt.Errorf("book.db checksum: %w", err)
		}
	}

	res, err := ingestBookDB(store, dbPath, outDir, libraryDir, &manifest, opts.IntoWorkID)
	if err != nil {
		// The ingest already rolled back what it added; also drop the extracted
		// files so no orphaned folder sits on disk looking like a book.
		os.RemoveAll(outDir)
		return nil, err
	}
	res.Edition = edition
	return res, nil
}

// editionDirName names the per-edition directory for an .abook from the narration
// it carries (read from its book.db): "<origin>[-<voice>][-<label>]", e.g.
// "tts_kokoro-af_heart" or "narrator_recording". A text-only file is "text"; a
// file whose narration cannot be read falls back to its content version so two
// unknowns still never share a directory.
func editionDirName(zr *zip.Reader, contentVersion string) (string, error) {
	data, err := readFromZip(zr, "book.db")
	if err != nil {
		return "", fmt.Errorf("read book.db: %w", err)
	}
	tmp, err := os.CreateTemp("", "abook-edition-*.db")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	bdb, err := sql.Open("sqlite", tmpPath+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer bdb.Close()
	var origin, album, label string
	hasAudio := false
	rows, err := bdb.Query(`SELECT origin, album, edition FROM books WHERE asset_path IS NOT NULL AND asset_path != '' ORDER BY id`)
	if err == nil {
		for rows.Next() {
			var o, a, l string
			if rows.Scan(&o, &a, &l) == nil {
				hasAudio = true
				if origin == "" {
					origin, album, label = o, a, l
				}
			}
		}
		rows.Close()
	}
	if !hasAudio {
		return "text", nil
	}
	parts := []string{}
	for _, p := range []string{origin, album, label} {
		if p = editionSlug(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		if cv := sanitizeFilename(contentVersion); cv != "" {
			return "edition-" + cv, nil
		}
		return "edition", nil
	}
	return strings.Join(parts, "-"), nil
}

// ingestBookDB opens the carved book.db and copies its rows into the monolith
// under a fresh work id, remapping book ids as it goes. On ANY failure after the
// work row is created it rolls that row back (named-return + defer), so a
// half-finished import never leaves a partial "broken book" in the library.
func ingestBookDB(store *db.Store, dbPath, outDir, libraryDir string, manifest *Manifest, intoWorkID int64) (res *ImportResult, err error) {
	bdb, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open book.db: %w", err)
	}
	defer bdb.Close()

	res = &ImportResult{}
	var newWorkID int64
	var addedBooks []int64 // rollback set when adding to an EXISTING work
	if intoWorkID != 0 {
		newWorkID = intoWorkID
		defer func() {
			if err != nil {
				for _, id := range addedBooks {
					store.DeleteBook(id) // only what THIS import added; the work stays
				}
			}
		}()
	} else {
		newWorkID, err = store.CreateWork(manifest.Title, manifest.Author)
		if err != nil {
			return nil, fmt.Errorf("create work: %w", err)
		}
		defer func() {
			if err != nil {
				store.DeleteWork(newWorkID) // roll back the partial work on any later failure
			}
		}()
	}
	res.WorkID = newWorkID

	// works row → series metadata (the rest is already on the new work).
	var series string
	var seriesIdx float64
	bdb.QueryRow(`SELECT series, series_index FROM works LIMIT 1`).Scan(&series, &seriesIdx)
	if series != "" {
		store.SetSeries(newWorkID, series, seriesIdx)
	}

	// books → remap old book id to new server id.
	bookRemap := map[int64]int64{}
	rows, err := bdb.Query(`
		SELECT id, filename, format, media_type, title, author, album,
		       duration, start_sec, origin, visibility, edition, asset_path
		FROM books`)
	if err != nil {
		return nil, fmt.Errorf("read books: %w", err)
	}
	type bookrow struct {
		oldID                                             int64
		filename, format, mediaType, title, author, album string
		origin, visibility, edition                       string
		duration, startSec                                float64
		assetPath                                         sql.NullString
	}
	var books []bookrow
	for rows.Next() {
		var b bookrow
		if err := rows.Scan(&b.oldID, &b.filename, &b.format, &b.mediaType, &b.title,
			&b.author, &b.album, &b.duration, &b.startSec, &b.origin, &b.visibility,
			&b.edition, &b.assetPath); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan book: %w", err)
		}
		books = append(books, b)
	}
	rows.Close()

	// Adding to an existing work: a text source IDENTICAL to one the work already
	// holds (same format, chapter count, total words — the same publisher EPUB
	// shipped in both narrations' files) is REUSED: its old id remaps onto the
	// existing book so the incoming alignments/links point at it, and its content
	// rows (chapters/paragraphs/chunks/bookmarks) are not copied twice.
	skipContent := map[int64]bool{}
	if intoWorkID != 0 {
		if existing, gerr := store.GetWork(intoWorkID); gerr == nil && existing != nil {
			for _, b := range books {
				if b.assetPath.Valid && b.assetPath.String != "" {
					continue // audio is never deduped — it IS the new edition
				}
				var n int
				var words int
				bdb.QueryRow(`SELECT COUNT(*), COALESCE(SUM(word_count),0) FROM chapters WHERE book_id = ?`, b.oldID).Scan(&n, &words)
				for _, tf := range existing.TextFiles {
					if tf.Format != b.format {
						continue
					}
					en, _ := store.ChapterCount(tf.ID)
					ew := 0
					if chs, cerr := store.ListChapters(tf.ID); cerr == nil {
						for _, ch := range chs {
							ew += ch.WordCount
						}
					}
					if en == n && ew == words && n > 0 {
						bookRemap[b.oldID] = tf.ID
						skipContent[b.oldID] = true
						res.ReusedTexts++
						break
					}
				}
			}
		}
	}

	for _, b := range books {
		if skipContent[b.oldID] {
			continue
		}
		// Path: extracted audio file for audio sources; a synthetic unique
		// path for text sources (their content lives in chapters).
		var path string
		if b.assetPath.Valid && b.assetPath.String != "" {
			path = filepath.Join(outDir, b.assetPath.String)
		} else {
			path = filepath.Join(outDir, fmt.Sprintf("text-book-%d.abook-text", b.oldID))
		}
		var size int64
		if fi, err := os.Stat(path); err == nil {
			size = fi.Size()
		}
		if err := store.UpsertBook(db.Book{
			WorkID:     newWorkID,
			Path:       path,
			Filename:   b.filename,
			Format:     b.format,
			MediaType:  b.mediaType,
			SizeBytes:  size,
			Title:      b.title,
			Author:     b.author,
			Album:      b.album,
			Duration:   b.duration,
			StartSec:   b.startSec,
			Origin:     b.origin,
			Visibility: b.visibility,
			Edition:    b.edition,
		}); err != nil {
			return nil, fmt.Errorf("upsert book: %w", err)
		}
		newID, err := bookIDByPath(store, path)
		if err != nil {
			return nil, err
		}
		bookRemap[b.oldID] = newID
		addedBooks = append(addedBooks, newID)
		res.AddedBooks++
	}

	// Content rows for reused texts are skipped (they already exist); links,
	// alignments and sync remap through onto the reused ids.
	if err := copyChapters(bdb, store, bookRemap, skipContent); err != nil {
		return nil, err
	}
	if err := copyParagraphs(bdb, store, bookRemap, skipContent); err != nil {
		return nil, err
	}
	if err := copyChunks(bdb, store, bookRemap, skipContent); err != nil {
		return nil, err
	}
	if err := copyChapterLinks(bdb, store, newWorkID, bookRemap); err != nil {
		return nil, err
	}
	if err := copyAlignments(bdb, store, newWorkID, bookRemap); err != nil {
		return nil, err
	}
	if err := copySync(bdb, store, newWorkID, bookRemap); err != nil {
		return nil, err
	}
	if err := copyBookmarks(bdb, store, newWorkID, bookRemap, skipContent); err != nil {
		return nil, err
	}

	store.StampVersions(newWorkID, BookDBSchemaVersion)
	// Preserve the manifest's generation stamp (StampVersions set it to "now"),
	// so a sideloaded work reports when it was produced — dedupe-by-generation.
	// Into an existing work: keep the NEWER of the two stamps.
	if manifest.ContentVersion != "" {
		keep := manifest.ContentVersion
		if intoWorkID != 0 {
			if _, cv, ok, _ := store.FindWorkByTitleAuthor(manifest.Title, manifest.Author); ok && cv > keep {
				keep = cv
			}
		}
		store.SetContentVersion(newWorkID, keep)
	}
	// Wire the bundled cover to where GET /api/works/{id}/cover serves from
	// ({libraryDir}/covers/work-{id}.jpg). The zip extracts it into outDir, but
	// without this copy an imported work — including the first-run sample, the
	// one book a newcomer sees — renders as a coverless tile. Best-effort: a
	// missing/broken cover must never fail an otherwise-good import.
	coverDst := filepath.Join(libraryDir, "covers", fmt.Sprintf("work-%d.jpg", newWorkID))
	if _, serr := os.Stat(coverDst); manifest.Assets.Cover != "" && (intoWorkID == 0 || serr != nil) { // keep an existing work's cover
		src := filepath.Join(outDir, manifest.Assets.Cover)
		if data, rerr := os.ReadFile(src); rerr == nil && len(data) > 0 {
			coversDir := filepath.Join(libraryDir, "covers")
			if mkerr := os.MkdirAll(coversDir, 0755); mkerr == nil {
				dst := filepath.Join(coversDir, fmt.Sprintf("work-%d.jpg", newWorkID))
				if werr := os.WriteFile(dst, data, 0644); werr != nil {
					log.Printf("abook import: cover wire failed for work %d: %v", newWorkID, werr)
				}
			}
		}
	}
	log.Printf("abook import: completed %q → work %d (%d books added, %d texts reused)", manifest.Title, newWorkID, res.AddedBooks, res.ReusedTexts)
	return res, nil
}

func copyChapters(bdb *sql.DB, store *db.Store, remap map[int64]int64, skip map[int64]bool) error {
	rows, err := bdb.Query(`SELECT book_id, index_num, title, src, content, content_html, word_count, start_sec, end_sec, confidence FROM chapters`)
	if err != nil {
		return fmt.Errorf("read chapters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch db.Chapter
		var oldBook int64
		if err := rows.Scan(&oldBook, &ch.Index, &ch.Title, &ch.Src, &ch.Content, &ch.ContentHTML, &ch.WordCount, &ch.StartSec, &ch.EndSec, &ch.Confidence); err != nil {
			return err
		}
		if skip[oldBook] {
			continue
		}
		ch.BookID = remap[oldBook]
		if err := store.InsertChapter(ch); err != nil {
			return fmt.Errorf("insert chapter: %w", err)
		}
	}
	return rows.Err()
}

func copyParagraphs(bdb *sql.DB, store *db.Store, remap map[int64]int64, skip map[int64]bool) error {
	rows, err := bdb.Query(`SELECT book_id, chapter_idx, paragraph_idx, word_start, word_end, text FROM paragraphs`)
	if err != nil {
		return fmt.Errorf("read paragraphs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p db.Paragraph
		var oldBook int64
		if err := rows.Scan(&oldBook, &p.ChapterIdx, &p.ParagraphIdx, &p.WordStart, &p.WordEnd, &p.Text); err != nil {
			return err
		}
		if skip[oldBook] {
			continue
		}
		p.BookID = remap[oldBook]
		if err := store.InsertParagraph(p); err != nil {
			return fmt.Errorf("insert paragraph: %w", err)
		}
	}
	return rows.Err()
}

func copyChunks(bdb *sql.DB, store *db.Store, remap map[int64]int64, skip map[int64]bool) error {
	rows, err := bdb.Query(`SELECT book_id, chapter_idx, chunk_idx, content, start_word, end_word, embedding FROM chunks`)
	if err != nil {
		return fmt.Errorf("read chunks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c db.Chunk
		var oldBook int64
		if err := rows.Scan(&oldBook, &c.ChapterIdx, &c.ChunkIdx, &c.Content, &c.StartWord, &c.EndWord, &c.Embedding); err != nil {
			return err
		}
		if skip[oldBook] {
			continue
		}
		c.BookID = remap[oldBook]
		if err := store.InsertChunk(c); err != nil {
			return fmt.Errorf("insert chunk: %w", err)
		}
	}
	return rows.Err()
}

func copyChapterLinks(bdb *sql.DB, store *db.Store, workID int64, remap map[int64]int64) error {
	rows, err := bdb.Query(`SELECT audio_book_id, audio_index, text_book_id, text_index, confidence FROM chapter_links`)
	if err != nil {
		return fmt.Errorf("read chapter_links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l db.ChapterLink
		var oldAudio, oldText int64
		if err := rows.Scan(&oldAudio, &l.AudioIndex, &oldText, &l.TextIndex, &l.Confidence); err != nil {
			return err
		}
		l.AudioBookID = remap[oldAudio]
		l.TextBookID = remap[oldText]
		if err := store.InsertChapterLink(workID, l); err != nil {
			return fmt.Errorf("insert chapter_link: %w", err)
		}
	}
	return rows.Err()
}

func copyAlignments(bdb *sql.DB, store *db.Store, workID int64, remap map[int64]int64) error {
	rows, err := bdb.Query(`SELECT from_book_id, to_book_id, unit, confidence, method, pairs FROM alignments`)
	if err != nil {
		return fmt.Errorf("read alignments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a db.Alignment
		var oldFrom, oldTo int64
		if err := rows.Scan(&oldFrom, &oldTo, &a.Unit, &a.Confidence, &a.Method, &a.Pairs); err != nil {
			return err
		}
		a.WorkID = workID
		a.FromBookID = remap[oldFrom]
		a.ToBookID = remap[oldTo]
		if err := store.SaveAlignment(a); err != nil {
			return fmt.Errorf("save alignment: %w", err)
		}
	}
	return rows.Err()
}

func copySync(bdb *sql.DB, store *db.Store, workID int64, remap map[int64]int64) error {
	rows, err := bdb.Query(`SELECT audio_book_id, chapter_idx, timestamps FROM sync`)
	if err != nil {
		return fmt.Errorf("read sync: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var oldAudio int64
		var chapterIdx int
		var timestamps string
		if err := rows.Scan(&oldAudio, &chapterIdx, &timestamps); err != nil {
			return err
		}
		if err := store.SaveSyncData(workID, remap[oldAudio], chapterIdx, timestamps); err != nil {
			return fmt.Errorf("save sync: %w", err)
		}
	}
	return rows.Err()
}

func copyBookmarks(bdb *sql.DB, store *db.Store, workID int64, remap map[int64]int64, skip map[int64]bool) error {
	rows, err := bdb.Query(`SELECT book_id, type, chapter_idx, position_secs, start_word, end_word, text_snippet, note, color FROM bookmarks`)
	if err != nil {
		return fmt.Errorf("read bookmarks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bm db.Bookmark
		var oldBook int64
		if err := rows.Scan(&oldBook, &bm.Type, &bm.ChapterIdx, &bm.PositionSecs, &bm.StartWord, &bm.EndWord, &bm.TextSnippet, &bm.Note, &bm.Color); err != nil {
			return err
		}
		bm.WorkID = workID
		if skip[oldBook] {
			continue
		}
		bm.BookID = remap[oldBook]
		// Imported annotations land with the primary reader (user 1).
		if _, err := store.CreateBookmark(bm, 1); err != nil {
			return fmt.Errorf("create bookmark: %w", err)
		}
	}
	return rows.Err()
}

// bookIDByPath returns the server id of the book with the given (unique) path.
func bookIDByPath(store *db.Store, path string) (int64, error) {
	books, err := store.ListBooks()
	if err != nil {
		return 0, err
	}
	for _, b := range books {
		if b.Path == path {
			return b.ID, nil
		}
	}
	return 0, fmt.Errorf("imported book not found by path %q", path)
}

func verifyChecksum(path, want string) error {
	want = strings.TrimPrefix(want, "sha256:")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("mismatch: got %s want %s", got, want)
	}
	return nil
}

// ReadManifest reads and parses just the manifest.json from a .abook without
// extracting the rest. Used to list an export set's identity/version stamps
// cheaply.
func ReadManifest(abookPath string) (*Manifest, error) {
	r, err := zip.OpenReader(abookPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := readFromZip(&r.Reader, "manifest.json")
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
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

// editionSlug makes a directory-safe, readable token from a narration field:
// lowercase ASCII letters/digits, runs of anything else collapsed to one "-".
// "Kokoro · Fable" → "kokoro-fable"; "A Christmas Carol" → "a-christmas-carol".
func editionSlug(s string) string {
	var out []rune
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
			dash = false
		default:
			if !dash && len(out) > 0 {
				out = append(out, '-')
				dash = true
			}
		}
	}
	return strings.TrimRight(string(out), "-")
}
