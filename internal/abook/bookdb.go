package abook

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/pj/abookify/internal/db"
)

// bookDBSchema is the DDL for a per-work book.db — the self-contained slice
// of the monolith that ships inside a .abook v2. Server primary keys are
// preserved (a single work's rows never collide), so every cross-reference
// (work_id, book_id, from_book_id, audio_book_id) stays valid on the device.
// Keep this in sync with BookDBSchemaVersion.
const bookDBSchema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE works (
	id                   INTEGER PRIMARY KEY,
	title                TEXT NOT NULL DEFAULT '',
	author               TEXT NOT NULL DEFAULT '',
	series               TEXT NOT NULL DEFAULT '',
	series_index         REAL NOT NULL DEFAULT 0,
	has_audio            INTEGER NOT NULL DEFAULT 0,
	has_text             INTEGER NOT NULL DEFAULT 0,
	display_text_book_id INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE books (
	id            INTEGER PRIMARY KEY,
	work_id       INTEGER NOT NULL,
	filename      TEXT NOT NULL DEFAULT '',
	format        TEXT NOT NULL DEFAULT '',
	media_type    TEXT NOT NULL DEFAULT '',
	title         TEXT NOT NULL DEFAULT '',
	author        TEXT NOT NULL DEFAULT '',
	album         TEXT NOT NULL DEFAULT '',
	duration      REAL NOT NULL DEFAULT 0,
	start_sec     REAL NOT NULL DEFAULT 0,
	origin        TEXT NOT NULL DEFAULT '',
	visibility    TEXT NOT NULL DEFAULT 'visible',
	edition       TEXT NOT NULL DEFAULT '',
	chapter_count INTEGER NOT NULL DEFAULT 0,
	-- in-zip path to this book's bundled audio ("audio/book-{id}.mp3"),
	-- or NULL for text sources (their content lives in the chapters table).
	asset_path    TEXT
);

CREATE TABLE chapters (
	id           INTEGER PRIMARY KEY,
	book_id      INTEGER NOT NULL,
	index_num    INTEGER NOT NULL,
	title        TEXT NOT NULL DEFAULT '',
	src          TEXT NOT NULL DEFAULT '',
	content      TEXT NOT NULL DEFAULT '',
	content_html TEXT NOT NULL DEFAULT '',
	word_count   INTEGER NOT NULL DEFAULT 0,
	start_sec    REAL NOT NULL DEFAULT 0,
	end_sec      REAL NOT NULL DEFAULT 0,
	confidence   REAL NOT NULL DEFAULT 0
);
CREATE INDEX idx_chapters_book ON chapters(book_id, index_num);

CREATE TABLE paragraphs (
	id            INTEGER PRIMARY KEY,
	book_id       INTEGER NOT NULL,
	chapter_idx   INTEGER NOT NULL,
	paragraph_idx INTEGER NOT NULL,
	word_start    INTEGER NOT NULL DEFAULT 0,
	word_end      INTEGER NOT NULL DEFAULT 0,
	text          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_paragraphs_book_chapter ON paragraphs(book_id, chapter_idx);

CREATE TABLE chapter_links (
	id            INTEGER PRIMARY KEY,
	work_id       INTEGER NOT NULL,
	audio_book_id INTEGER NOT NULL,
	audio_index   INTEGER NOT NULL,
	text_book_id  INTEGER NOT NULL,
	text_index    INTEGER NOT NULL,
	confidence    REAL NOT NULL DEFAULT 0
);

CREATE TABLE chunks (
	id          INTEGER PRIMARY KEY,
	book_id     INTEGER NOT NULL,
	chapter_idx INTEGER NOT NULL,
	chunk_idx   INTEGER NOT NULL,
	content     TEXT NOT NULL DEFAULT '',
	start_word  INTEGER NOT NULL DEFAULT 0,
	end_word    INTEGER NOT NULL DEFAULT 0,
	embedding   BLOB
);
CREATE INDEX idx_chunks_book ON chunks(book_id);

CREATE TABLE alignments (
	id           INTEGER PRIMARY KEY,
	work_id      INTEGER NOT NULL,
	from_book_id INTEGER NOT NULL,
	to_book_id   INTEGER NOT NULL,
	unit         TEXT NOT NULL DEFAULT 'word',
	confidence   REAL NOT NULL DEFAULT 0,
	method       TEXT NOT NULL DEFAULT '',
	pairs        TEXT NOT NULL DEFAULT '[]'
);

CREATE TABLE sync (
	id            INTEGER PRIMARY KEY,
	work_id       INTEGER NOT NULL,
	audio_book_id INTEGER NOT NULL,
	chapter_idx   INTEGER NOT NULL,
	timestamps    TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX idx_sync_audio ON sync(audio_book_id, chapter_idx);

CREATE TABLE bookmarks (
	id            INTEGER PRIMARY KEY,
	work_id       INTEGER NOT NULL,
	book_id       INTEGER NOT NULL,
	type          TEXT NOT NULL DEFAULT 'bookmark',
	chapter_idx   INTEGER NOT NULL DEFAULT 0,
	position_secs REAL NOT NULL DEFAULT 0,
	start_word    INTEGER NOT NULL DEFAULT 0,
	end_word      INTEGER NOT NULL DEFAULT 0,
	text_snippet  TEXT NOT NULL DEFAULT '',
	note          TEXT NOT NULL DEFAULT '',
	color         TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL DEFAULT ''
);
`

// WorkSummary is the denormalized listing/manifest summary for a work.
type WorkSummary struct {
	SourceKind  string
	CoveragePct *float64
	AlignMethod *string
	AlignUnit   *string
	// Provenance is a short human-readable line describing how this generation
	// was produced (audio source, alignment method, text source) so a .abook is
	// self-describing about WHICH generation it is.
	Provenance string
}

// SummarizeWork derives the listing badge fields (source_kind, coverage,
// method, unit) from the work's alignments. Mirrors the heuristic in the
// approved design: aligned > transcript > text-only > audio-only. On a query
// error it returns a zero summary (source_kind by media presence only) so
// callers that just want the badge don't have to handle errors.
func SummarizeWork(store *db.Store, work *db.Work) WorkSummary {
	var sum WorkSummary
	aligns, err := store.ListAlignmentsForWork(work.ID)
	if err != nil {
		aligns = nil
	}
	var best *db.Alignment
	for i := range aligns {
		if aligns[i].Confidence <= 0 {
			continue
		}
		if best == nil || aligns[i].Confidence > best.Confidence {
			best = &aligns[i]
		}
	}
	if best != nil {
		pct := best.Confidence * 100
		method := best.Method
		unit := best.Unit
		sum.CoveragePct = &pct
		sum.AlignMethod = &method
		sum.AlignUnit = &unit
	}

	switch {
	case work.HasAudio && work.HasText && best != nil:
		sum.SourceKind = "aligned"
	case work.HasAudio && work.HasText:
		sum.SourceKind = "transcript"
	case work.HasText:
		sum.SourceKind = "text-only"
	case work.HasAudio:
		sum.SourceKind = "audio-only"
	}
	sum.Provenance = provenanceLine(work, sum)
	return sum
}

// provenanceLine builds a short human-readable description of how this
// generation was produced — the audio source (TTS vs narration), the alignment
// method, and the text source — so a reader can tell a re-TTS'd or re-aligned
// .abook from an older one at a glance.
func provenanceLine(work *db.Work, sum WorkSummary) string {
	var parts []string
	// Audio source.
	if work.HasAudio {
		tts := false
		for _, b := range work.AudioFiles {
			if b.Origin == "tts_kokoro" || b.Origin == "tts_preprocessed" {
				tts = true
				break
			}
		}
		if tts {
			parts = append(parts, "TTS-generated audio (Kokoro)")
		} else {
			parts = append(parts, "narrated audio")
		}
	}
	// Alignment.
	if sum.AlignMethod != nil && *sum.AlignMethod != "" {
		unit := ""
		if sum.AlignUnit != nil && *sum.AlignUnit != "" {
			unit = "/" + *sum.AlignUnit
		}
		parts = append(parts, *sum.AlignMethod+unit+" alignment")
	}
	// Text source (prefer a visible publisher ebook over a transcript).
	text := ""
	for _, b := range work.TextFiles {
		if b.Visibility == "internal" {
			continue
		}
		switch b.Origin {
		case "publisher_epub", "publisher_mobi", "publisher_pdf", "user_upload":
			text = "publisher ebook"
		case "whisper_transcript":
			if text == "" {
				text = "Whisper transcript"
			}
		}
	}
	if text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, " · ")
}

// embeddingDimOf returns the vector dimension of the carved book.db's chunk
// embeddings (bytes/4 of the first non-null blob, float32), or 0 if none.
func embeddingDimOf(dbPath string) int {
	bdb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0
	}
	defer bdb.Close()
	var n int
	if err := bdb.QueryRow(
		`SELECT LENGTH(embedding) FROM chunks WHERE embedding IS NOT NULL AND LENGTH(embedding) > 0 LIMIT 1`,
	).Scan(&n); err != nil {
		return 0
	}
	return n / 4
}

// embedModelForDim names the embedding model that produces a given dimension,
// so a consumer embeds queries with a matching model. Covers the two providers
// the server supports (OpenAI / Ollama); "" for an unrecognized dim.
func embedModelForDim(dim int) string {
	switch dim {
	case 1536:
		return "text-embedding-3-small" // OpenAI
	case 768:
		return "nomic-embed-text" // Ollama
	default:
		return ""
	}
}

// buildBookDB writes a fresh book.db at dbPath containing the carved slice of
// the monolith for this work. assetPaths maps each audio book id to its
// intended in-zip path ("audio/book-{id}.mp3"); pass nil for text-only works.
// Embeddings are written only when includeEmbeddings is true.
func buildBookDB(store *db.Store, work *db.Work, sum WorkSummary, dbPath string, assetPaths map[int64]string, includeEmbeddings bool) error {
	// Plain rollback-journal (not WAL): after commit+close the single book.db
	// file is complete with no -wal/-shm sidecars to bundle. The device can
	// switch the file to WAL itself when it opens it for reading.
	bdb, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open book.db: %w", err)
	}
	defer bdb.Close()
	bdb.SetMaxOpenConns(1)

	if _, err := bdb.Exec(bookDBSchema); err != nil {
		return fmt.Errorf("create book.db schema: %w", err)
	}

	tx, err := bdb.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// meta
	meta := [][2]string{
		{"schema_version", fmt.Sprintf("%d", BookDBSchemaVersion)},
		{"content_version", work.ContentVersion},
		{"generated_at", time.Now().UTC().Format(time.RFC3339)},
		{"generator", generator},
		{"work_id", fmt.Sprintf("%d", work.ID)},
		{"title", work.Title},
		{"author", work.Author},
		{"language", language},
		{"source_kind", sum.SourceKind},
	}
	if sum.CoveragePct != nil {
		meta = append(meta,
			[2]string{"coverage_pct", fmt.Sprintf("%.4f", *sum.CoveragePct)},
			[2]string{"align_method", *sum.AlignMethod},
			[2]string{"align_unit", *sum.AlignUnit},
		)
	}
	for _, kv := range meta {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)`, kv[0], kv[1]); err != nil {
			return fmt.Errorf("insert meta %q: %w", kv[0], err)
		}
	}

	// works (single row)
	if _, err := tx.Exec(`
		INSERT INTO works(id, title, author, series, series_index, has_audio, has_text, display_text_book_id)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		work.ID, work.Title, work.Author, work.Series, work.SeriesIndex,
		b2i(work.HasAudio), b2i(work.HasText), work.DisplayTextBookID,
	); err != nil {
		return fmt.Errorf("insert work: %w", err)
	}

	// books + their chapters/paragraphs/chunks
	allBooks := append(append([]db.Book{}, work.AudioFiles...), work.TextFiles...)
	for _, bk := range allBooks {
		chCount, _ := store.ChapterCount(bk.ID)
		var assetPath any
		if p, ok := assetPaths[bk.ID]; ok {
			assetPath = p
		}
		if _, err := tx.Exec(`
			INSERT INTO books(id, work_id, filename, format, media_type, title, author, album,
				duration, start_sec, origin, visibility, edition, chapter_count, asset_path)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			bk.ID, bk.WorkID, bk.Filename, bk.Format, bk.MediaType, bk.Title, bk.Author, bk.Album,
			bk.Duration, bk.StartSec, bk.Origin, bk.Visibility, bk.Edition, chCount, assetPath,
		); err != nil {
			return fmt.Errorf("insert book %d: %w", bk.ID, err)
		}

		chapters, err := store.ListChapters(bk.ID)
		if err != nil {
			return fmt.Errorf("list chapters for book %d: %w", bk.ID, err)
		}
		for _, ch := range chapters {
			full, err := store.GetChapterContent(bk.ID, ch.Index)
			if err != nil {
				return fmt.Errorf("get chapter content %d/%d: %w", bk.ID, ch.Index, err)
			}
			content, html := "", ""
			if full != nil {
				content, html = full.Content, full.ContentHTML
			}
			if _, err := tx.Exec(`
				INSERT INTO chapters(id, book_id, index_num, title, src, content, content_html,
					word_count, start_sec, end_sec, confidence)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ch.ID, ch.BookID, ch.Index, ch.Title, ch.Src, content, html,
				ch.WordCount, ch.StartSec, ch.EndSec, ch.Confidence,
			); err != nil {
				return fmt.Errorf("insert chapter %d: %w", ch.ID, err)
			}

			paras, err := store.ListParagraphs(bk.ID, ch.Index)
			if err != nil {
				return fmt.Errorf("list paragraphs %d/%d: %w", bk.ID, ch.Index, err)
			}
			for _, p := range paras {
				if _, err := tx.Exec(`
					INSERT INTO paragraphs(id, book_id, chapter_idx, paragraph_idx, word_start, word_end, text)
					VALUES(?, ?, ?, ?, ?, ?, ?)`,
					p.ID, p.BookID, p.ChapterIdx, p.ParagraphIdx, p.WordStart, p.WordEnd, p.Text,
				); err != nil {
					return fmt.Errorf("insert paragraph %d: %w", p.ID, err)
				}
			}
		}

		chunks, err := store.ListChunks(bk.ID)
		if err != nil {
			return fmt.Errorf("list chunks for book %d: %w", bk.ID, err)
		}
		for _, c := range chunks {
			var emb any
			if includeEmbeddings && len(c.Embedding) > 0 {
				emb = c.Embedding
			}
			if _, err := tx.Exec(`
				INSERT INTO chunks(id, book_id, chapter_idx, chunk_idx, content, start_word, end_word, embedding)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
				c.ID, c.BookID, c.ChapterIdx, c.ChunkIdx, c.Content, c.StartWord, c.EndWord, emb,
			); err != nil {
				return fmt.Errorf("insert chunk %d: %w", c.ID, err)
			}
		}
	}

	// chapter_links
	links, err := store.GetChapterLinks(work.ID)
	if err != nil {
		return fmt.Errorf("chapter links: %w", err)
	}
	for _, l := range links {
		if _, err := tx.Exec(`
			INSERT INTO chapter_links(work_id, audio_book_id, audio_index, text_book_id, text_index, confidence)
			VALUES(?, ?, ?, ?, ?, ?)`,
			work.ID, l.AudioBookID, l.AudioIndex, l.TextBookID, l.TextIndex, l.Confidence,
		); err != nil {
			return fmt.Errorf("insert chapter_link: %w", err)
		}
	}

	// alignments
	aligns, err := store.ListAlignmentsForWork(work.ID)
	if err != nil {
		return fmt.Errorf("alignments: %w", err)
	}
	for _, a := range aligns {
		if _, err := tx.Exec(`
			INSERT INTO alignments(id, work_id, from_book_id, to_book_id, unit, confidence, method, pairs)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.WorkID, a.FromBookID, a.ToBookID, a.Unit, a.Confidence, a.Method, a.Pairs,
		); err != nil {
			return fmt.Errorf("insert alignment %d: %w", a.ID, err)
		}
	}

	// sync
	syncRows, err := store.ListSyncForWork(work.ID)
	if err != nil {
		return fmt.Errorf("sync rows: %w", err)
	}
	for _, sr := range syncRows {
		if _, err := tx.Exec(`
			INSERT INTO sync(id, work_id, audio_book_id, chapter_idx, timestamps)
			VALUES(?, ?, ?, ?, ?)`,
			sr.ID, work.ID, sr.AudioBookID, sr.ChapterIdx, sr.Timestamps,
		); err != nil {
			return fmt.Errorf("insert sync %d: %w", sr.ID, err)
		}
	}

	// bookmarks (+ highlights, distinguished by type)
	bookmarks, err := store.ListBookmarks(work.ID)
	if err != nil {
		return fmt.Errorf("bookmarks: %w", err)
	}
	for _, bm := range bookmarks {
		if _, err := tx.Exec(`
			INSERT INTO bookmarks(id, work_id, book_id, type, chapter_idx, position_secs,
				start_word, end_word, text_snippet, note, color, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			bm.ID, bm.WorkID, bm.BookID, bm.Type, bm.ChapterIdx, bm.PositionSecs,
			bm.StartWord, bm.EndWord, bm.TextSnippet, bm.Note, bm.Color, bm.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert bookmark %d: %w", bm.ID, err)
		}
	}

	return tx.Commit()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
