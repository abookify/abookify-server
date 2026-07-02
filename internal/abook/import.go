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
func Import(store *db.Store, abookPath string, libraryDir string) error {
	r, err := zip.OpenReader(abookPath)
	if err != nil {
		return fmt.Errorf("open abook: %w", err)
	}
	defer r.Close()

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
	if manifest.Version != 2 {
		return fmt.Errorf("unsupported .abook version %d (expected 2)", manifest.Version)
	}

	log.Printf("abook import: %q by %s (v2)", manifest.Title, manifest.Author)

	safeName := sanitizeFilename(manifest.Title)
	outDir := filepath.Join(libraryDir, "abooks", safeName)
	if err := os.MkdirAll(filepath.Join(outDir, "audio"), 0755); err != nil {
		return fmt.Errorf("create out dir: %w", err)
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
			continue
		}
		io.Copy(out, rc)
		out.Close()
		rc.Close()
	}

	dbPath := filepath.Join(outDir, manifest.Assets.DB)
	if want := manifest.Checksums["book.db"]; want != "" {
		if err := verifyChecksum(dbPath, want); err != nil {
			return fmt.Errorf("book.db checksum: %w", err)
		}
	}

	return ingestBookDB(store, dbPath, outDir, &manifest)
}

// ingestBookDB opens the carved book.db and copies its rows into the monolith
// under a fresh work id, remapping book ids as it goes.
func ingestBookDB(store *db.Store, dbPath, outDir string, manifest *Manifest) error {
	bdb, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return fmt.Errorf("open book.db: %w", err)
	}
	defer bdb.Close()

	newWorkID, err := store.CreateWork(manifest.Title, manifest.Author)
	if err != nil {
		return fmt.Errorf("create work: %w", err)
	}

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
		return fmt.Errorf("read books: %w", err)
	}
	type bookrow struct {
		oldID                                                 int64
		filename, format, mediaType, title, author, album     string
		origin, visibility, edition                           string
		duration, startSec                                    float64
		assetPath                                             sql.NullString
	}
	var books []bookrow
	for rows.Next() {
		var b bookrow
		if err := rows.Scan(&b.oldID, &b.filename, &b.format, &b.mediaType, &b.title,
			&b.author, &b.album, &b.duration, &b.startSec, &b.origin, &b.visibility,
			&b.edition, &b.assetPath); err != nil {
			rows.Close()
			return fmt.Errorf("scan book: %w", err)
		}
		books = append(books, b)
	}
	rows.Close()

	for _, b := range books {
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
			WorkID:    newWorkID,
			Path:      path,
			Filename:  b.filename,
			Format:    b.format,
			MediaType: b.mediaType,
			SizeBytes: size,
			Title:     b.title,
			Author:    b.author,
			Album:     b.album,
			Duration:  b.duration,
			StartSec:  b.startSec,
			Origin:    b.origin,
			Visibility: b.visibility,
			Edition:   b.edition,
		}); err != nil {
			return fmt.Errorf("upsert book: %w", err)
		}
		newID, err := bookIDByPath(store, path)
		if err != nil {
			return err
		}
		bookRemap[b.oldID] = newID
	}

	// chapters
	if err := copyChapters(bdb, store, bookRemap); err != nil {
		return err
	}
	// paragraphs
	if err := copyParagraphs(bdb, store, bookRemap); err != nil {
		return err
	}
	// chunks
	if err := copyChunks(bdb, store, bookRemap); err != nil {
		return err
	}
	// chapter_links
	if err := copyChapterLinks(bdb, store, newWorkID, bookRemap); err != nil {
		return err
	}
	// alignments
	if err := copyAlignments(bdb, store, newWorkID, bookRemap); err != nil {
		return err
	}
	// sync
	if err := copySync(bdb, store, newWorkID, bookRemap); err != nil {
		return err
	}
	// bookmarks
	if err := copyBookmarks(bdb, store, newWorkID, bookRemap); err != nil {
		return err
	}

	store.StampVersions(newWorkID, BookDBSchemaVersion)
	// Preserve the manifest's generation stamp (StampVersions set it to "now"),
	// so a sideloaded work reports when it was produced — dedupe-by-generation.
	if manifest.ContentVersion != "" {
		store.SetContentVersion(newWorkID, manifest.ContentVersion)
	}
	log.Printf("abook import: completed %q → work %d (%d books)", manifest.Title, newWorkID, len(bookRemap))
	return nil
}

func copyChapters(bdb *sql.DB, store *db.Store, remap map[int64]int64) error {
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
		ch.BookID = remap[oldBook]
		if err := store.InsertChapter(ch); err != nil {
			return fmt.Errorf("insert chapter: %w", err)
		}
	}
	return rows.Err()
}

func copyParagraphs(bdb *sql.DB, store *db.Store, remap map[int64]int64) error {
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
		p.BookID = remap[oldBook]
		if err := store.InsertParagraph(p); err != nil {
			return fmt.Errorf("insert paragraph: %w", err)
		}
	}
	return rows.Err()
}

func copyChunks(bdb *sql.DB, store *db.Store, remap map[int64]int64) error {
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

func copyBookmarks(bdb *sql.DB, store *db.Store, workID int64, remap map[int64]int64) error {
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
		bm.BookID = remap[oldBook]
		if _, err := store.CreateBookmark(bm); err != nil {
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
