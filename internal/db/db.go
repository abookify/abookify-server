package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pj/abookify/internal/applog"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Work struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	HasAudio    bool   `json:"has_audio"`
	HasText     bool   `json:"has_text"`
	// Series metadata — extracted from EPUB Calibre tags or title patterns.
	// Empty series string = standalone work.
	Series      string  `json:"series,omitempty"`
	SeriesIndex float64 `json:"series_index,omitempty"` // supports fractional (e.g. 2.5 for novellas)
	// DisplayTextBookID is the user's per-work override of the display
	// resolver. 0 = no override (resolver picks by OriginAuthority).
	DisplayTextBookID int64 `json:"display_text_book_id,omitempty"`
	AudioFiles   []Book         `json:"audio_files,omitempty"`
	TextFiles    []Book         `json:"text_files,omitempty"`
	ChapterLinks []ChapterLink  `json:"chapter_links,omitempty"`
	TotalSize    int64          `json:"total_size"`
	// Local-first sync stamps. SchemaVersion is the book.db shape this work
	// would export under; ContentVersion is the RFC3339 UTC time of its last
	// (re)process. See StampVersions and design/local-first-sync.md.
	SchemaVersion  int    `json:"schema_version"`
	ContentVersion string `json:"content_version"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type Chunk struct {
	ID         int64   `json:"id"`
	BookID     int64   `json:"book_id"`
	ChapterIdx int     `json:"chapter_idx"`
	ChunkIdx   int     `json:"chunk_idx"`
	Content    string  `json:"content"`
	StartWord  int     `json:"start_word"`
	EndWord    int     `json:"end_word"`
	Embedding  []byte  `json:"-"` // binary blob, not in JSON
}

// AlignmentPair is one matched region between two sources. Stored as JSON
// inside the Alignment.Pairs blob. Positions are word indices within the
// source's chapter (from_chapter_idx + from_start → from_end). Confidence
// is per-pair; the Alignment-level confidence is the aggregate.
type AlignmentPair struct {
	FromChapter int     `json:"fc"` // chapter idx in from_book
	FromStart   int     `json:"fs"` // word start in that chapter
	FromEnd     int     `json:"fe"` // word end (exclusive)
	ToChapter   int     `json:"tc"` // chapter idx in to_book
	ToStart     int     `json:"ts"` // word start in that chapter
	ToEnd       int     `json:"te"` // word end (exclusive)
	Confidence  float64 `json:"c"`  // 0.0–1.0 for this specific pair
}

// Alignment is a pairwise relation between two source materials (books).
// The pairs blob holds []AlignmentPair as JSON. Method describes how the
// alignment was computed ("whisper-native", "edit-distance", "manual").
type Alignment struct {
	ID         int64     `json:"id"`
	WorkID     int64     `json:"work_id"`
	FromBookID int64     `json:"from_book_id"`
	ToBookID   int64     `json:"to_book_id"`
	Unit       string    `json:"unit"`       // "word" | "paragraph" | "sentence"
	Confidence float64   `json:"confidence"` // aggregate
	Method     string    `json:"method"`
	Pairs      string    `json:"pairs"` // JSON blob of []AlignmentPair
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Paragraph struct {
	ID           int64  `json:"id"`
	BookID       int64  `json:"book_id"`
	ChapterIdx   int    `json:"chapter_idx"`
	ParagraphIdx int    `json:"paragraph_idx"`
	WordStart    int    `json:"word_start"`
	WordEnd      int    `json:"word_end"`
	Text         string `json:"text,omitempty"`
}

type Chapter struct {
	ID        int64  `json:"id"`
	BookID    int64  `json:"book_id"`
	Index     int    `json:"index"`
	Title     string `json:"title"`
	Src       string `json:"src,omitempty"`
	WordCount int    `json:"word_count"`
	// Time range within the audio book (0 for text chapters).
	StartSec   float64 `json:"start_sec,omitempty"`
	EndSec     float64 `json:"end_sec,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// Content is plaintext — used for search, alignment, word counting.
	// Only loaded on demand, not in list responses.
	Content string `json:"content,omitempty"`
	// ContentHTML is sanitized HTML from the source EPUB. When present,
	// the reader renders this instead of Content for visual fidelity
	// (headings, emphasis, paragraphs). Empty for transcripts.
	ContentHTML string `json:"content_html,omitempty"`
}

type Book struct {
	ID        int64     `json:"id"`
	WorkID    int64     `json:"work_id"`
	Path      string    `json:"path"`
	Filename  string    `json:"filename"`
	Format    string    `json:"format"`
	MediaType string    `json:"media_type"` // "audio" or "text"
	SizeBytes int64     `json:"size_bytes"`
	Title     string    `json:"title,omitempty"`
	Author    string    `json:"author,omitempty"`
	Album     string    `json:"album,omitempty"`
	Duration     float64   `json:"duration_secs,omitempty"`
	// StartSec is the file's start position on the concatenated book
	// timeline (from the sidecar's sources[] when available). For
	// single-file books this is always 0; for multi-file books it's
	// the ground-truth offset Whisper used when transcribing, which
	// avoids drift from summing metadata durations on the client.
	StartSec     float64   `json:"start_sec,omitempty"`
	ChapterCount int       `json:"chapter_count,omitempty"`
	// Origin describes how this source material was produced. Used by the
	// display resolver to pick the highest-authority source for the reader.
	// Values: publisher_epub, publisher_mobi, publisher_pdf, author_recording,
	// narrator_recording, librivox, tts_kokoro, whisper_transcript,
	// tts_preprocessed, user_upload (default).
	Origin     string    `json:"origin,omitempty"`
	// Visibility: "visible" (shown in UI, default) or "internal" (pipeline
	// intermediates like TTS-preprocessed text that should never be shown).
	Visibility string    `json:"visibility,omitempty"`
	// Edition labels a named variant within the same work. For audio:
	// "LibriVox - Jane Doe", "Audible - John Smith". For text:
	// "Original", "Annotated", "Spanish translation". Empty = default edition.
	// Works with multiple editions expose an edition picker in the UI.
	Edition    string    `json:"edition,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	// modernc.org/sqlite uses the _pragma=name(value) DSN form, NOT the
	// mattn-style _journal_mode=WAL. The old DSN was silently ignored, so
	// the db ran in rollback-journal mode where a writer takes an
	// exclusive lock and readers get SQLITE_BUSY. WAL lets readers run
	// concurrently with a writer; busy_timeout waits out brief contention.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Serialize all access through a single connection. busy_timeout makes
	// most writer-vs-writer contention wait it out, but it cannot cover a
	// true deadlock (two connections each holding SHARED, both wanting
	// RESERVED) — SQLite returns SQLITE_BUSY immediately without invoking
	// the busy handler. A scanner/sidecar-import burst hit exactly that.
	// With one connection there is never internal contention. This is safe
	// here because no code holds a transaction or open *sql.Rows while
	// issuing a second query, and the cost is nil for a single-user server.
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS works (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			title        TEXT NOT NULL DEFAULT '',
			author       TEXT NOT NULL DEFAULT '',
			series       TEXT NOT NULL DEFAULT '',
			series_index REAL NOT NULL DEFAULT 0,
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		-- idx_works_series is created after the ALTER TABLE migrations below,
		-- because pre-existing DBs created before the series column was added
		-- won't have the column at this point — CREATE TABLE IF NOT EXISTS is
		-- a no-op for them, so the ALTER below is what actually installs it.

		CREATE TABLE IF NOT EXISTS books (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			work_id    INTEGER NOT NULL DEFAULT 0,
			path       TEXT NOT NULL UNIQUE,
			filename   TEXT NOT NULL,
			format     TEXT NOT NULL,
			media_type TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL DEFAULT 0,
			title      TEXT NOT NULL DEFAULT '',
			author     TEXT NOT NULL DEFAULT '',
			album      TEXT NOT NULL DEFAULT '',
			duration   REAL NOT NULL DEFAULT 0,
			origin     TEXT NOT NULL DEFAULT 'user_upload',
			visibility TEXT NOT NULL DEFAULT 'visible',
			edition    TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (work_id) REFERENCES works(id)
		);

		CREATE INDEX IF NOT EXISTS idx_books_format ON books(format);
		CREATE INDEX IF NOT EXISTS idx_books_work_id ON books(work_id);
		CREATE INDEX IF NOT EXISTS idx_books_media_type ON books(media_type);

		CREATE TABLE IF NOT EXISTS chapters (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			book_id      INTEGER NOT NULL,
			index_num    INTEGER NOT NULL,
			title        TEXT NOT NULL DEFAULT '',
			src          TEXT NOT NULL DEFAULT '',
			content      TEXT NOT NULL DEFAULT '',
			content_html TEXT NOT NULL DEFAULT '',
			word_count   INTEGER NOT NULL DEFAULT 0,
			start_sec    REAL NOT NULL DEFAULT 0,
			end_sec      REAL NOT NULL DEFAULT 0,
			confidence   REAL NOT NULL DEFAULT 0,
			FOREIGN KEY (book_id) REFERENCES books(id),
			UNIQUE(book_id, index_num)
		);

		CREATE INDEX IF NOT EXISTS idx_chapters_book_id ON chapters(book_id);

		CREATE TABLE IF NOT EXISTS paragraphs (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			book_id        INTEGER NOT NULL,
			chapter_idx    INTEGER NOT NULL,
			paragraph_idx  INTEGER NOT NULL,
			word_start     INTEGER NOT NULL DEFAULT 0,
			word_end       INTEGER NOT NULL DEFAULT 0,
			text           TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (book_id) REFERENCES books(id),
			UNIQUE(book_id, chapter_idx, paragraph_idx)
		);

		CREATE INDEX IF NOT EXISTS idx_paragraphs_book_chapter ON paragraphs(book_id, chapter_idx);

		CREATE TABLE IF NOT EXISTS chunks (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			book_id     INTEGER NOT NULL,
			chapter_idx INTEGER NOT NULL,
			chunk_idx   INTEGER NOT NULL,
			content     TEXT NOT NULL,
			start_word  INTEGER NOT NULL DEFAULT 0,
			end_word    INTEGER NOT NULL DEFAULT 0,
			embedding   BLOB,
			FOREIGN KEY (book_id) REFERENCES books(id),
			UNIQUE(book_id, chapter_idx, chunk_idx)
		);

		CREATE INDEX IF NOT EXISTS idx_chunks_book_id ON chunks(book_id);

		CREATE TABLE IF NOT EXISTS chapter_links (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			work_id         INTEGER NOT NULL,
			audio_book_id   INTEGER NOT NULL,
			audio_index     INTEGER NOT NULL,
			text_book_id    INTEGER NOT NULL,
			text_index      INTEGER NOT NULL,
			confidence       REAL NOT NULL DEFAULT 0,
			FOREIGN KEY (work_id) REFERENCES works(id),
			UNIQUE(work_id, audio_index)
		);

		CREATE INDEX IF NOT EXISTS idx_chapter_links_work ON chapter_links(work_id);

		CREATE TABLE IF NOT EXISTS playback_positions (
			work_id       INTEGER NOT NULL,
			book_id       INTEGER NOT NULL,
			file_index    INTEGER NOT NULL DEFAULT 0,
			position_secs REAL NOT NULL DEFAULT 0,
			updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (work_id),
			FOREIGN KEY (work_id) REFERENCES works(id),
			FOREIGN KEY (book_id) REFERENCES books(id)
		);

		CREATE TABLE IF NOT EXISTS bookmarks (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			work_id       INTEGER NOT NULL,
			book_id       INTEGER NOT NULL,
			type          TEXT NOT NULL DEFAULT 'bookmark',
			chapter_idx   INTEGER NOT NULL DEFAULT 0,
			position_secs REAL NOT NULL DEFAULT 0,
			start_word    INTEGER NOT NULL DEFAULT 0,
			end_word      INTEGER NOT NULL DEFAULT 0,
			text_snippet  TEXT NOT NULL DEFAULT '',
			note          TEXT NOT NULL DEFAULT '',
			color         TEXT NOT NULL DEFAULT '#f0a500',
			created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (work_id) REFERENCES works(id),
			FOREIGN KEY (book_id) REFERENCES books(id)
		);

		CREATE INDEX IF NOT EXISTS idx_bookmarks_work ON bookmarks(work_id);

		CREATE TABLE IF NOT EXISTS sync_data (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			work_id     INTEGER NOT NULL,
			audio_book_id INTEGER NOT NULL,
			chapter_idx INTEGER NOT NULL,
			timestamps  TEXT NOT NULL DEFAULT '[]',
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(work_id, audio_book_id, chapter_idx)
		);

		CREATE INDEX IF NOT EXISTS idx_sync_work ON sync_data(work_id);

		-- Pairwise alignments between peer sources (audio, transcript, epub, etc.).
		-- Each row links two books and stores a JSON blob of mapped position pairs.
		-- Composable: audio→transcript + transcript→epub = audio→epub.
		CREATE TABLE IF NOT EXISTS alignments (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			work_id       INTEGER NOT NULL,
			from_book_id  INTEGER NOT NULL,
			to_book_id    INTEGER NOT NULL,
			unit          TEXT NOT NULL DEFAULT 'word',
			confidence    REAL NOT NULL DEFAULT 0,
			method        TEXT NOT NULL DEFAULT '',
			pairs         TEXT NOT NULL DEFAULT '[]',
			created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (work_id) REFERENCES works(id),
			FOREIGN KEY (from_book_id) REFERENCES books(id),
			FOREIGN KEY (to_book_id) REFERENCES books(id),
			UNIQUE(from_book_id, to_book_id, unit)
		);

		CREATE INDEX IF NOT EXISTS idx_alignments_work ON alignments(work_id);
		CREATE INDEX IF NOT EXISTS idx_alignments_from ON alignments(from_book_id);
		CREATE INDEX IF NOT EXISTS idx_alignments_to   ON alignments(to_book_id);

		CREATE TABLE IF NOT EXISTS jobs (
			id           TEXT PRIMARY KEY,
			work_id      INTEGER NOT NULL,
			type         TEXT NOT NULL,
			status       TEXT NOT NULL DEFAULT 'running',
			progress     REAL NOT NULL DEFAULT 0,
			current_step TEXT NOT NULL DEFAULT '',
			error        TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		-- Structured application logs (#214). A bounded recent window
		-- (~24h, pruned hourly) backing the in-UI System Console so job
		-- failures and pipeline errors are debuggable without Claude Code.
		-- fields is a JSON object ("" when none). Written async/batched by
		-- internal/applog; never on a request hot path.
		CREATE TABLE IF NOT EXISTS logs (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			ts        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			level     TEXT NOT NULL DEFAULT 'info',
			component TEXT NOT NULL DEFAULT '',
			job_id    TEXT NOT NULL DEFAULT '',
			work_id   INTEGER NOT NULL DEFAULT 0,
			message   TEXT NOT NULL DEFAULT '',
			fields    TEXT NOT NULL DEFAULT ''
		);

		CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts DESC);
		CREATE INDEX IF NOT EXISTS idx_logs_component ON logs(component);
		CREATE INDEX IF NOT EXISTS idx_logs_job ON logs(job_id);

		CREATE TABLE IF NOT EXISTS playback_events (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			work_id   INTEGER NOT NULL,
			event     TEXT NOT NULL,
			seconds   REAL NOT NULL DEFAULT 0,
			date      TEXT NOT NULL DEFAULT (date('now')),
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (work_id) REFERENCES works(id)
		);

		CREATE INDEX IF NOT EXISTS idx_playback_events_date ON playback_events(date);
		CREATE INDEX IF NOT EXISTS idx_playback_events_work ON playback_events(work_id);

		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		);

		-- Login sessions for optional username/password auth (#197).
		-- Opaque random tokens minted at login (or embedded in a pairing
		-- QR), validated per request. A DB table (not in-memory) so 30-day
		-- tokens survive the frequent server restarts. Rows are deleted on
		-- logout and lazily purged once expired.
		CREATE TABLE IF NOT EXISTS auth_sessions (
			token      TEXT PRIMARY KEY,
			username   TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at DATETIME NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_auth_sessions_expires ON auth_sessions(expires_at);

		-- Q&A chat sessions: a session is a multi-turn conversation
		-- scoped to one work. A book can have many parallel sessions
		-- (one per topic, draft, or open tab). Title is auto-derived
		-- from the first user message but can be renamed.
		CREATE TABLE IF NOT EXISTS qa_sessions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			work_id    INTEGER NOT NULL,
			title      TEXT NOT NULL DEFAULT 'New chat',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (work_id) REFERENCES works(id)
		);

		CREATE INDEX IF NOT EXISTS idx_qa_sessions_work ON qa_sessions(work_id, updated_at DESC);

		-- One message per turn. role is "user" or "assistant".
		-- citations_json stores []llm.Citation for assistant turns; "" for user.
		CREATE TABLE IF NOT EXISTS qa_messages (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id     INTEGER NOT NULL,
			role           TEXT    NOT NULL,
			content        TEXT    NOT NULL,
			citations_json TEXT    NOT NULL DEFAULT '',
			created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (session_id) REFERENCES qa_sessions(id) ON DELETE CASCADE
		);

		CREATE INDEX IF NOT EXISTS idx_qa_messages_session ON qa_messages(session_id, id);

		-- Cast of characters (EXPERIMENTAL, #booknlp). Derived from an EPUB
		-- text book by the optional booknlp service; additive and fully
		-- removable. A work with no rows here simply has no cast (the UI
		-- degrades gracefully). Re-extraction replaces a book's rows.
		CREATE TABLE IF NOT EXISTS characters (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			work_id       INTEGER NOT NULL,
			book_id       INTEGER NOT NULL,   -- the EPUB text book the cast came from
			name          TEXT NOT NULL DEFAULT '',  -- canonical display name
			aliases       TEXT NOT NULL DEFAULT '[]', -- JSON array of variant names
			gender        TEXT NOT NULL DEFAULT '',   -- BookNLP argmax gender or ''
			mention_count INTEGER NOT NULL DEFAULT 0,
			rank          INTEGER NOT NULL DEFAULT 0, -- 0-based, by mention_count desc
			created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (work_id) REFERENCES works(id),
			FOREIGN KEY (book_id) REFERENCES books(id)
		);

		CREATE INDEX IF NOT EXISTS idx_characters_work ON characters(work_id);
		CREATE INDEX IF NOT EXISTS idx_characters_book ON characters(book_id);

		-- Per-character mention positions (token offset within the book).
		-- Stored for future "where does X appear" features; the MVP cast
		-- panel reads only the characters table.
		CREATE TABLE IF NOT EXISTS character_mentions (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			character_id INTEGER NOT NULL,
			book_id      INTEGER NOT NULL,
			offset       INTEGER NOT NULL DEFAULT 0,
			surface      TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (character_id) REFERENCES characters(id)
		);

		CREATE INDEX IF NOT EXISTS idx_character_mentions_char ON character_mentions(character_id);
	`)
	if err != nil {
		return err
	}

	// Additive column migrations. sqlite has no IF NOT EXISTS for ADD COLUMN,
	// so we swallow "duplicate column" errors — everything else surfaces.
	for _, stmt := range []string{
		`ALTER TABLE chapters ADD COLUMN start_sec  REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE chapters ADD COLUMN end_sec    REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE chapters ADD COLUMN confidence REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE books ADD COLUMN origin     TEXT NOT NULL DEFAULT 'user_upload'`,
		`ALTER TABLE books ADD COLUMN visibility TEXT NOT NULL DEFAULT 'visible'`,
		`ALTER TABLE chapters ADD COLUMN content_html TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE works ADD COLUMN series       TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE works ADD COLUMN series_index REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE playback_positions ADD COLUMN device_id   TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE playback_positions ADD COLUMN device_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE books ADD COLUMN edition TEXT NOT NULL DEFAULT ''`,
		// JSON list of transcription gap spans (silently-skipped Whisper
		// chunks). Empty string = no gap analysis done yet; "[]" = analyzed,
		// none found. Populated by library.DetectTranscriptionGaps during
		// sidecar import.
		`ALTER TABLE books ADD COLUMN transcription_gaps TEXT NOT NULL DEFAULT ''`,
		// Position of this audio file on the concatenated book timeline.
		// Sourced from sidecar sources[].start_sec during import; 0 for
		// single-file books. The client uses this to compute karaoke
		// offsets accurately instead of summing metadata durations
		// (which drift by milliseconds per file).
		`ALTER TABLE books ADD COLUMN start_sec REAL NOT NULL DEFAULT 0`,
		// Per-work display-source override (#100/#101). 0 = use authority
		// resolver default. When set, the reader shows this text book
		// regardless of OriginAuthority — covers cases where the user
		// wants the transcript even though a publisher EPUB is present
		// (e.g. comparing narration line-by-line to the printed text).
		`ALTER TABLE works ADD COLUMN display_text_book_id INTEGER NOT NULL DEFAULT 0`,
		// Local-first sync version stamps (design/local-first-sync.md).
		// schema_version = shape of the exported book.db (a code constant,
		// abook.BookDBSchemaVersion); content_version = RFC3339 UTC timestamp
		// of the last (re)process. Mobile compares these to its installed
		// copy to drive "Update available". Both are stamped by StampVersions.
		`ALTER TABLE works ADD COLUMN schema_version  INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE works ADD COLUMN content_version TEXT    NOT NULL DEFAULT ''`,
		// Per-message Q&A scope snapshot — the QueryScope that shaped
		// retrieval for this turn. Empty = whole book (legacy / default).
		// Lets the chat history show "asked about Chapter 6" badges
		// after the fact so users can reason about answers, and lets
		// follow-up turns inherit the prior scope as a default.
		`ALTER TABLE qa_messages ADD COLUMN scope_json TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migration %q: %w", stmt, err)
		}
	}

	// Indexes on post-migration columns. Kept here (not in the CREATE TABLE
	// block) so pre-existing DBs that missed the original CREATE get the
	// column added first, then the index built against it.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_works_series ON works(series)`); err != nil {
		return fmt.Errorf("create idx_works_series: %w", err)
	}

	// Backfill origin for books created before the origin column existed.
	// Scanner-ingested files get format-derived defaults; pipeline outputs
	// get their correct origin tags.
	for _, backfill := range []string{
		`UPDATE books SET origin = 'publisher_epub' WHERE origin = 'user_upload' AND format = 'epub'`,
		`UPDATE books SET origin = 'publisher_pdf'  WHERE origin = 'user_upload' AND format = 'pdf'`,
		`UPDATE books SET origin = 'narrator_recording' WHERE origin = 'user_upload' AND format IN ('mp3','m4b','m4a','flac','aac') AND path NOT LIKE 'generated://%'`,
		`UPDATE books SET origin = 'whisper_transcript' WHERE origin = 'user_upload' AND format = 'transcript'`,
		`UPDATE books SET origin = 'tts_kokoro' WHERE origin = 'user_upload' AND path LIKE 'generated://%' AND media_type = 'audio'`,
	} {
		db.Exec(backfill) // best-effort; no-op once origins are correct
	}

	// Backfill content_version for works created before the column existed,
	// so the version endpoint / catalog never report an empty stamp. Uses
	// updated_at as the best available proxy for "last processed". No-op once
	// every row has a non-empty stamp (set by StampVersions thereafter).
	db.Exec(`UPDATE works SET content_version = strftime('%Y-%m-%dT%H:%M:%SZ', updated_at) WHERE content_version = ''`)
	return nil
}

type PlaybackPosition struct {
	WorkID       int64   `json:"work_id"`
	BookID       int64   `json:"book_id"`
	FileIndex    int     `json:"file_index"`
	PositionSecs float64 `json:"position_secs"`
	DeviceID     string  `json:"device_id,omitempty"`   // which device saved this
	DeviceName   string  `json:"device_name,omitempty"` // human-readable, e.g. "PJ's iPhone"
	UpdatedAt    string  `json:"updated_at"`
}

func (s *Store) SavePosition(pos PlaybackPosition) error {
	_, err := s.db.Exec(`
		INSERT INTO playback_positions (work_id, book_id, file_index, position_secs, device_id, device_name, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(work_id) DO UPDATE SET
			book_id = excluded.book_id,
			file_index = excluded.file_index,
			position_secs = excluded.position_secs,
			device_id = CASE WHEN excluded.device_id != '' THEN excluded.device_id ELSE playback_positions.device_id END,
			device_name = CASE WHEN excluded.device_name != '' THEN excluded.device_name ELSE playback_positions.device_name END,
			updated_at = CURRENT_TIMESTAMP
	`, pos.WorkID, pos.BookID, pos.FileIndex, pos.PositionSecs, pos.DeviceID, pos.DeviceName)
	return err
}

func (s *Store) GetPosition(workID int64) (*PlaybackPosition, error) {
	var pos PlaybackPosition
	err := s.db.QueryRow(`
		SELECT work_id, book_id, file_index, position_secs, device_id, device_name, updated_at
		FROM playback_positions WHERE work_id = ?
	`, workID).Scan(&pos.WorkID, &pos.BookID, &pos.FileIndex, &pos.PositionSecs, &pos.DeviceID, &pos.DeviceName, &pos.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pos, nil
}

// --- Playback analytics ---

// RecordPlayback logs a listening event (e.g. "played 300 seconds of work 9").
// Events are bucketed by date for daily/weekly/monthly aggregation.
func (s *Store) RecordPlayback(workID int64, event string, seconds float64) error {
	_, err := s.db.Exec(`
		INSERT INTO playback_events (work_id, event, seconds)
		VALUES (?, ?, ?)
	`, workID, event, seconds)
	return err
}

// PlaybackStats returns aggregated listening time per day for the last N days.
type DailyStats struct {
	Date    string  `json:"date"`
	Seconds float64 `json:"seconds"`
	Works   int     `json:"works"` // distinct works listened to
}

func (s *Store) PlaybackStatsByDay(days int) ([]DailyStats, error) {
	rows, err := s.db.Query(`
		SELECT date, SUM(seconds), COUNT(DISTINCT work_id)
		FROM playback_events
		WHERE date >= date('now', '-' || ? || ' days')
		GROUP BY date
		ORDER BY date
	`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []DailyStats
	for rows.Next() {
		var d DailyStats
		if err := rows.Scan(&d.Date, &d.Seconds, &d.Works); err != nil {
			return nil, err
		}
		stats = append(stats, d)
	}
	return stats, rows.Err()
}

// PlaybackTotalSeconds returns total listening time across all time.
func (s *Store) PlaybackTotalSeconds() (float64, error) {
	var total float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(seconds), 0) FROM playback_events`).Scan(&total)
	return total, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

func (s *Store) GetSetting(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (s *Store) GetAllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

type ChapterLink struct {
	AudioBookID int64   `json:"audio_book_id"`
	AudioIndex  int     `json:"audio_index"`
	TextBookID  int64   `json:"text_book_id"`
	TextIndex   int     `json:"text_index"`
	Confidence  float64 `json:"confidence"`
}

func (s *Store) InsertChapterLink(workID int64, link ChapterLink) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO chapter_links (work_id, audio_book_id, audio_index, text_book_id, text_index, confidence)
		VALUES (?, ?, ?, ?, ?, ?)
	`, workID, link.AudioBookID, link.AudioIndex, link.TextBookID, link.TextIndex, link.Confidence)
	return err
}

// DeleteChapterLinksByWork clears all chapter_links for a work. Used before
// rebuilding the link set so stale entries don't linger when chapter counts change.
func (s *Store) DeleteChapterLinksByWork(workID int64) error {
	_, err := s.db.Exec(`DELETE FROM chapter_links WHERE work_id = ?`, workID)
	return err
}

func (s *Store) GetChapterLinks(workID int64) ([]ChapterLink, error) {
	rows, err := s.db.Query(`
		SELECT audio_book_id, audio_index, text_book_id, text_index, confidence
		FROM chapter_links WHERE work_id = ? ORDER BY audio_index
	`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []ChapterLink
	for rows.Next() {
		var l ChapterLink
		if err := rows.Scan(&l.AudioBookID, &l.AudioIndex, &l.TextBookID, &l.TextIndex, &l.Confidence); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

func (s *Store) UpsertBook(b Book) error {
	if b.Origin == "" {
		b.Origin = "user_upload"
	}
	if b.Visibility == "" {
		b.Visibility = "visible"
	}
	_, err := s.db.Exec(`
		INSERT INTO books (work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, edition, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(path) DO UPDATE SET
			work_id    = CASE WHEN excluded.work_id != 0 THEN excluded.work_id ELSE books.work_id END,
			filename   = excluded.filename,
			format     = excluded.format,
			media_type = excluded.media_type,
			size_bytes = excluded.size_bytes,
			title      = CASE WHEN excluded.title != '' THEN excluded.title ELSE books.title END,
			author     = CASE WHEN excluded.author != '' THEN excluded.author ELSE books.author END,
			album      = CASE WHEN excluded.album != '' THEN excluded.album ELSE books.album END,
			duration   = CASE WHEN excluded.duration > 0 THEN excluded.duration ELSE books.duration END,
			origin     = CASE WHEN excluded.origin != 'user_upload' THEN excluded.origin ELSE books.origin END,
			visibility = CASE WHEN excluded.visibility != 'visible' THEN excluded.visibility ELSE books.visibility END,
			edition    = CASE WHEN excluded.edition != '' THEN excluded.edition ELSE books.edition END,
			updated_at = CURRENT_TIMESTAMP
	`, b.WorkID, b.Path, b.Filename, b.Format, b.MediaType, b.SizeBytes, b.Title, b.Author, b.Album, b.Duration, b.Origin, b.Visibility, b.Edition)
	return err
}

func (s *Store) CreateWork(title, author string) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO works (title, author) VALUES (?, ?)
	`, title, author)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetSeries updates just the series + series_index for a work.
// Used by EPUB metadata extraction during scan.
func (s *Store) SetSeries(id int64, series string, index float64) error {
	_, err := s.db.Exec(`
		UPDATE works SET series = ?, series_index = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, series, index, id)
	return err
}

// SetDisplayTextBook records the user's per-work choice of which text
// source the reader should show. Pass 0 to clear the override and let
// the authority resolver decide. The caller is responsible for
// verifying the book actually belongs to this work + has visibility
// != "internal"; SQL doesn't enforce that.
func (s *Store) SetDisplayTextBook(workID, bookID int64) error {
	_, err := s.db.Exec(`UPDATE works SET display_text_book_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, bookID, workID)
	return err
}

// StampVersions records that a work was just (re)processed: it sets
// schema_version to the given current book.db shape and content_version to
// the current UTC time (RFC3339). Call this at the end of any operation that
// changes a work's exportable data — alignment, metadata edit, chapter
// extraction, TTS/STT regeneration — so mobile's "Update available" check
// (GET /api/works/{id}/version) sees the new stamp. See local-first-sync.md.
func (s *Store) StampVersions(workID int64, schemaVersion int) error {
	_, err := s.db.Exec(`
		UPDATE works
		SET schema_version = ?,
		    content_version = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'),
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, schemaVersion, workID)
	return err
}

// GetVersions returns the work's (schema_version, content_version) stamps.
// found is false when no such work exists. This is the read side of the
// cheap update-check endpoint.
func (s *Store) GetVersions(workID int64) (schemaVersion int, contentVersion string, found bool, err error) {
	err = s.db.QueryRow(
		`SELECT schema_version, content_version FROM works WHERE id = ?`, workID,
	).Scan(&schemaVersion, &contentVersion)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return schemaVersion, contentVersion, true, nil
}

// UpdateWork updates the title and author of a work. Empty strings are
// treated as "no change" (keeps existing value).
func (s *Store) UpdateWork(id int64, title, author string) error {
	if title != "" && author != "" {
		_, err := s.db.Exec(`UPDATE works SET title = ?, author = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, title, author, id)
		return err
	}
	if title != "" {
		_, err := s.db.Exec(`UPDATE works SET title = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, title, id)
		return err
	}
	if author != "" {
		_, err := s.db.Exec(`UPDATE works SET author = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, author, id)
		return err
	}
	return nil
}

func (s *Store) AssignBooksToWork(workID int64, bookIDs []int64) error {
	for _, id := range bookIDs {
		if _, err := s.db.Exec(`UPDATE books SET work_id = ? WHERE id = ?`, workID, id); err != nil {
			return err
		}
	}
	return nil
}

// FindWorkByAudioDir returns the work_id of an existing work that already owns
// audio books whose path starts with dirPrefix+"/". Returns 0 if no such work.
// Used by the matcher to avoid creating duplicate works when a rescan of an
// already-known directory surfaces some files as unassigned (e.g. after a
// partial failure or metadata re-read).
func (s *Store) FindWorkByAudioDir(dirPrefix string) (int64, error) {
	var workID int64
	err := s.db.QueryRow(`
		SELECT work_id FROM books
		WHERE media_type = 'audio' AND work_id != 0 AND path LIKE ? || '/%'
		LIMIT 1
	`, dirPrefix).Scan(&workID)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return workID, nil
}

func (s *Store) ListBooks() ([]Book, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, edition, start_sec, created_at, updated_at
		FROM books ORDER BY title, filename
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.WorkID, &b.Path, &b.Filename, &b.Format, &b.MediaType, &b.SizeBytes,
			&b.Title, &b.Author, &b.Album, &b.Duration, &b.Origin, &b.Visibility, &b.Edition, &b.StartSec, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

func (s *Store) GetBook(id int64) (*Book, error) {
	var b Book
	err := s.db.QueryRow(`
		SELECT id, work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, edition, start_sec, created_at, updated_at
		FROM books WHERE id = ?
	`, id).Scan(&b.ID, &b.WorkID, &b.Path, &b.Filename, &b.Format, &b.MediaType, &b.SizeBytes,
		&b.Title, &b.Author, &b.Album, &b.Duration, &b.Origin, &b.Visibility, &b.Edition, &b.StartSec, &b.CreatedAt, &b.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) ListWorks() ([]Work, error) {
	rows, err := s.db.Query(`
		SELECT id, title, author, series, series_index, display_text_book_id, schema_version, content_version, created_at, updated_at
		FROM works ORDER BY series, series_index, title
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var works []Work
	for rows.Next() {
		var w Work
		if err := rows.Scan(&w.ID, &w.Title, &w.Author, &w.Series, &w.SeriesIndex, &w.DisplayTextBookID, &w.SchemaVersion, &w.ContentVersion, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		works = append(works, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load books and chapter links for each work
	for i := range works {
		books, err := s.booksByWork(works[i].ID)
		if err != nil {
			return nil, err
		}
		for _, b := range books {
			works[i].TotalSize += b.SizeBytes
			if b.MediaType == "audio" {
				works[i].HasAudio = true
				works[i].AudioFiles = append(works[i].AudioFiles, b)
			} else {
				works[i].HasText = true
				works[i].TextFiles = append(works[i].TextFiles, b)
			}
		}
		links, err := s.GetChapterLinks(works[i].ID)
		if err == nil && len(links) > 0 {
			works[i].ChapterLinks = links
		}
	}

	return works, nil
}

func (s *Store) GetWork(id int64) (*Work, error) {
	var w Work
	err := s.db.QueryRow(`
		SELECT id, title, author, series, series_index, display_text_book_id, schema_version, content_version, created_at, updated_at FROM works WHERE id = ?
	`, id).Scan(&w.ID, &w.Title, &w.Author, &w.Series, &w.SeriesIndex, &w.DisplayTextBookID, &w.SchemaVersion, &w.ContentVersion, &w.CreatedAt, &w.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	books, err := s.booksByWork(w.ID)
	if err != nil {
		return nil, err
	}
	for _, b := range books {
		w.TotalSize += b.SizeBytes
		if b.MediaType == "audio" {
			w.HasAudio = true
			w.AudioFiles = append(w.AudioFiles, b)
		} else {
			w.HasText = true
			w.TextFiles = append(w.TextFiles, b)
		}
	}

	links, err := s.GetChapterLinks(w.ID)
	if err == nil && len(links) > 0 {
		w.ChapterLinks = links
	}

	return &w, nil
}

func (s *Store) booksByWork(workID int64) ([]Book, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, edition, start_sec, created_at, updated_at
		FROM books WHERE work_id = ? ORDER BY filename
	`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.WorkID, &b.Path, &b.Filename, &b.Format, &b.MediaType, &b.SizeBytes,
			&b.Title, &b.Author, &b.Album, &b.Duration, &b.Origin, &b.Visibility, &b.Edition, &b.StartSec, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Populate chapter counts for text books
	for i := range books {
		if books[i].MediaType == "text" {
			count, err := s.ChapterCount(books[i].ID)
			if err == nil {
				books[i].ChapterCount = count
			}
		}
	}

	return books, nil
}

// OriginAuthority returns a numeric score for a source origin — higher is
// more authoritative. Used by the display resolver to pick the best source
// to show in the reader when multiple text (or audio) sources exist.
func OriginAuthority(origin string) int {
	switch origin {
	case "publisher_epub":
		return 100
	case "publisher_mobi":
		return 95
	case "publisher_pdf":
		return 90
	case "author_recording":
		return 100
	case "narrator_recording":
		return 80
	case "librivox":
		return 60
	case "tts_kokoro":
		return 30
	case "whisper_transcript":
		return 20
	case "tts_preprocessed":
		return 10
	case "user_upload":
		return 50
	default:
		return 50
	}
}

// UnassignedBooks returns books not yet linked to a work.
func (s *Store) UnassignedBooks() ([]Book, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, edition, start_sec, created_at, updated_at
		FROM books WHERE work_id = 0 ORDER BY title, filename
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.WorkID, &b.Path, &b.Filename, &b.Format, &b.MediaType, &b.SizeBytes,
			&b.Title, &b.Author, &b.Album, &b.Duration, &b.Origin, &b.Visibility, &b.Edition, &b.StartSec, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

func (s *Store) InsertChapter(ch Chapter) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO chapters (book_id, index_num, title, src, content, content_html, word_count, start_sec, end_sec, confidence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, ch.BookID, ch.Index, ch.Title, ch.Src, ch.Content, ch.ContentHTML, ch.WordCount, ch.StartSec, ch.EndSec, ch.Confidence)
	return err
}

// DeleteChaptersByBook removes all chapter rows for a given book.
// Used when re-running chapter detection so we don't leave stale entries.
func (s *Store) DeleteChaptersByBook(bookID int64) error {
	_, err := s.db.Exec(`DELETE FROM chapters WHERE book_id = ?`, bookID)
	return err
}

// UpdateChapterTitle replaces just the title for an existing chapter row.
// Used by the LLM-fallback labeler to fill in bare "Chapter N" titles
// without touching the chapter's content or timestamps.
func (s *Store) UpdateChapterTitle(bookID int64, index int, title string) error {
	_, err := s.db.Exec(`UPDATE chapters SET title = ? WHERE book_id = ? AND index_num = ?`,
		title, bookID, index)
	return err
}

// SetBookStartSec writes the file's position on the concatenated
// book timeline. Called during sidecar import once we know the
// sources[] offsets — see PopulateAudioFileOffsets.
func (s *Store) SetBookStartSec(bookID int64, startSec float64) error {
	_, err := s.db.Exec(`UPDATE books SET start_sec = ? WHERE id = ?`, startSec, bookID)
	return err
}

// SaveTranscriptionGaps persists the JSON-encoded list of audio spans
// where Whisper produced no transcribed output. Empty list ("[]") is a
// valid value — it means analysis ran and found no gaps.
func (s *Store) SaveTranscriptionGaps(bookID int64, gapsJSON string) error {
	_, err := s.db.Exec(`UPDATE books SET transcription_gaps = ? WHERE id = ?`,
		gapsJSON, bookID)
	return err
}

// GetTranscriptionGaps returns the JSON string previously persisted by
// SaveTranscriptionGaps. Empty string means "not analyzed yet".
func (s *Store) GetTranscriptionGaps(bookID int64) (string, error) {
	var out string
	err := s.db.QueryRow(`SELECT transcription_gaps FROM books WHERE id = ?`, bookID).Scan(&out)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return out, err
}

// HasChaptersMissingHTML returns true if any chapter for this book has
// empty content_html. Used on boot to detect pre-#102 data that needs
// re-extraction from the source EPUB.
func (s *Store) HasChaptersMissingHTML(bookID int64) bool {
	var count int
	s.db.QueryRow(`
		SELECT COUNT(*) FROM chapters
		WHERE book_id = ? AND content_html = '' AND content != ''
	`, bookID).Scan(&count)
	return count > 0
}

func (s *Store) ChapterCount(bookID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM chapters WHERE book_id = ?`, bookID).Scan(&count)
	return count, err
}

// ListChapters returns chapters for a book without content (for listing).
func (s *Store) ListChapters(bookID int64) ([]Chapter, error) {
	rows, err := s.db.Query(`
		SELECT id, book_id, index_num, title, src, word_count, start_sec, end_sec, confidence
		FROM chapters WHERE book_id = ? ORDER BY index_num
	`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chapters []Chapter
	for rows.Next() {
		var ch Chapter
		if err := rows.Scan(&ch.ID, &ch.BookID, &ch.Index, &ch.Title, &ch.Src, &ch.WordCount, &ch.StartSec, &ch.EndSec, &ch.Confidence); err != nil {
			return nil, err
		}
		chapters = append(chapters, ch)
	}
	return chapters, rows.Err()
}

// GetChapterContent returns a single chapter with its content (plain + HTML).
func (s *Store) GetChapterContent(bookID int64, index int) (*Chapter, error) {
	var ch Chapter
	err := s.db.QueryRow(`
		SELECT id, book_id, index_num, title, src, content, content_html, word_count, start_sec, end_sec, confidence
		FROM chapters WHERE book_id = ? AND index_num = ?
	`, bookID, index).Scan(&ch.ID, &ch.BookID, &ch.Index, &ch.Title, &ch.Src, &ch.Content, &ch.ContentHTML, &ch.WordCount, &ch.StartSec, &ch.EndSec, &ch.Confidence)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

// InsertParagraph writes a paragraph row. Idempotent via
// (book_id, chapter_idx, paragraph_idx) unique constraint.
func (s *Store) InsertParagraph(p Paragraph) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO paragraphs (book_id, chapter_idx, paragraph_idx, word_start, word_end, text)
		VALUES (?, ?, ?, ?, ?, ?)
	`, p.BookID, p.ChapterIdx, p.ParagraphIdx, p.WordStart, p.WordEnd, p.Text)
	return err
}

// DeleteParagraphsByBook removes all paragraph rows for a book. Used before
// re-populating (e.g. after transcript re-split or chapter re-extraction).
func (s *Store) DeleteParagraphsByBook(bookID int64) error {
	_, err := s.db.Exec(`DELETE FROM paragraphs WHERE book_id = ?`, bookID)
	return err
}

// ReplaceParagraphsForBook atomically replaces every paragraph for a book.
// Used for fast bulk population; one transaction regardless of size.
func (s *Store) ReplaceParagraphsForBook(bookID int64, paragraphs []Paragraph) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM paragraphs WHERE book_id = ?`, bookID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
		INSERT INTO paragraphs (book_id, chapter_idx, paragraph_idx, word_start, word_end, text)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range paragraphs {
		if _, err := stmt.Exec(p.BookID, p.ChapterIdx, p.ParagraphIdx, p.WordStart, p.WordEnd, p.Text); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListParagraphs returns all paragraphs for a chapter in order.
func (s *Store) ListParagraphs(bookID int64, chapterIdx int) ([]Paragraph, error) {
	rows, err := s.db.Query(`
		SELECT id, book_id, chapter_idx, paragraph_idx, word_start, word_end, text
		FROM paragraphs
		WHERE book_id = ? AND chapter_idx = ?
		ORDER BY paragraph_idx
	`, bookID, chapterIdx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Paragraph
	for rows.Next() {
		var p Paragraph
		if err := rows.Scan(&p.ID, &p.BookID, &p.ChapterIdx, &p.ParagraphIdx,
			&p.WordStart, &p.WordEnd, &p.Text); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ParagraphCount returns the total paragraphs stored for a book (across all chapters).
func (s *Store) ParagraphCount(bookID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM paragraphs WHERE book_id = ?`, bookID).Scan(&count)
	return count, err
}

func (s *Store) InsertChunk(c Chunk) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO chunks (book_id, chapter_idx, chunk_idx, content, start_word, end_word, embedding)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, c.BookID, c.ChapterIdx, c.ChunkIdx, c.Content, c.StartWord, c.EndWord, c.Embedding)
	return err
}

func (s *Store) ChunkCount(bookID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE book_id = ?`, bookID).Scan(&count)
	return count, err
}

// EmbeddingCoverage returns the total chunk count and the count with a
// non-NULL embedding column. Used by the Settings page to surface how
// much of the library's RAG corpus is vector-searchable vs the
// keyword-only fallback. Both numbers are repo-wide (not per-book).
func (s *Store) EmbeddingCoverage() (total, withEmbedding int, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COUNT(embedding) FROM chunks`).Scan(&total, &withEmbedding)
	return
}

// DeleteChunksByBook removes every chunk row for a book — used when a
// reprocess pass invalidates chapter boundaries and the chunks need to
// be rebuilt against new chapter splits.
func (s *Store) DeleteChunksByBook(bookID int64) error {
	_, err := s.db.Exec(`DELETE FROM chunks WHERE book_id = ?`, bookID)
	return err
}

func (s *Store) SearchChunks(bookID int64, query string) ([]Chunk, error) {
	// Simple keyword search for now; will be replaced with vector similarity
	rows, err := s.db.Query(`
		SELECT id, book_id, chapter_idx, chunk_idx, content, start_word, end_word
		FROM chunks WHERE book_id = ? AND content LIKE '%' || ? || '%'
		ORDER BY chapter_idx, chunk_idx
		LIMIT 20
	`, bookID, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.BookID, &c.ChapterIdx, &c.ChunkIdx,
			&c.Content, &c.StartWord, &c.EndWord); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// ListChunks returns all chunks for a book WITH their embedding blobs.
// Used for batch embedding population and vector search.
func (s *Store) ListChunks(bookID int64) ([]Chunk, error) {
	rows, err := s.db.Query(`
		SELECT id, book_id, chapter_idx, chunk_idx, content, start_word, end_word, embedding
		FROM chunks WHERE book_id = ? ORDER BY chapter_idx, chunk_idx
	`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.BookID, &c.ChapterIdx, &c.ChunkIdx,
			&c.Content, &c.StartWord, &c.EndWord, &c.Embedding); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// UpdateChunkEmbedding updates just the embedding blob for a chunk.
func (s *Store) UpdateChunkEmbedding(chunkID int64, embedding []byte) error {
	_, err := s.db.Exec(`UPDATE chunks SET embedding = ? WHERE id = ?`, embedding, chunkID)
	return err
}

// ListAllChunksWithEmbeddings returns all chunks across ALL books that have
// non-null embeddings. Used for global vector search in Q&A.
func (s *Store) ListAllChunksWithEmbeddings(workID int64) ([]Chunk, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.book_id, c.chapter_idx, c.chunk_idx, c.content, c.start_word, c.end_word, c.embedding
		FROM chunks c
		JOIN books b ON b.id = c.book_id
		WHERE b.work_id = ? AND c.embedding IS NOT NULL AND length(c.embedding) > 0
		ORDER BY c.chapter_idx, c.chunk_idx
	`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.BookID, &c.ChapterIdx, &c.ChunkIdx,
			&c.Content, &c.StartWord, &c.EndWord, &c.Embedding); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// Character is one entry in a work's cast of characters (EXPERIMENTAL).
// Derived from an EPUB by the optional booknlp service. Aliases is the set of
// surface-name variants BookNLP clustered under this character.
type Character struct {
	ID           int64    `json:"id"`
	WorkID       int64    `json:"work_id"`
	BookID       int64    `json:"book_id"`
	Name         string   `json:"name"`
	Aliases      []string `json:"aliases"`
	Gender       string   `json:"gender,omitempty"`
	MentionCount int      `json:"mention_count"`
	Rank         int      `json:"rank"`
}

// ReplaceCharactersForBook atomically swaps a book's cast (delete + insert).
// Re-extraction is idempotent: the prior cast for this book is removed first,
// including its mentions. Characters are stored in the given order with rank
// assigned by slice position.
func (s *Store) ReplaceCharactersForBook(workID, bookID int64, chars []Character) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Drop mentions for this book's existing characters, then the characters.
	if _, err := tx.Exec(`DELETE FROM character_mentions WHERE character_id IN (SELECT id FROM characters WHERE book_id = ?)`, bookID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM characters WHERE book_id = ?`, bookID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
		INSERT INTO characters (work_id, book_id, name, aliases, gender, mention_count, rank)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, c := range chars {
		aliases, _ := json.Marshal(c.Aliases)
		if c.Aliases == nil {
			aliases = []byte("[]")
		}
		if _, err := stmt.Exec(workID, bookID, c.Name, string(aliases), c.Gender, c.MentionCount, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListCharactersForWork returns the cast for a work (all its EPUB books),
// ordered by rank. Empty slice when no cast has been extracted.
func (s *Store) ListCharactersForWork(workID int64) ([]Character, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, book_id, name, aliases, gender, mention_count, rank
		FROM characters WHERE work_id = ? ORDER BY rank, mention_count DESC
	`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Character
	for rows.Next() {
		var c Character
		var aliasesJSON string
		if err := rows.Scan(&c.ID, &c.WorkID, &c.BookID, &c.Name, &aliasesJSON, &c.Gender, &c.MentionCount, &c.Rank); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(aliasesJSON), &c.Aliases)
		if c.Aliases == nil {
			c.Aliases = []string{}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type Bookmark struct {
	ID           int64   `json:"id"`
	WorkID       int64   `json:"work_id"`
	BookID       int64   `json:"book_id"`
	Type         string  `json:"type"` // "bookmark" or "highlight"
	ChapterIdx   int     `json:"chapter_idx"`
	PositionSecs float64 `json:"position_secs,omitempty"`
	StartWord    int     `json:"start_word,omitempty"`
	EndWord      int     `json:"end_word,omitempty"`
	TextSnippet  string  `json:"text_snippet,omitempty"`
	Note         string  `json:"note,omitempty"`
	Color        string  `json:"color,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

// UpdateBookmark updates the mutable fields on an existing bookmark
// (note, color). Immutable fields like position/word range stay.
func (s *Store) UpdateBookmark(id int64, note, color string) error {
	if note != "" && color != "" {
		_, err := s.db.Exec(`UPDATE bookmarks SET note = ?, color = ? WHERE id = ?`, note, color, id)
		return err
	}
	if note != "" {
		_, err := s.db.Exec(`UPDATE bookmarks SET note = ? WHERE id = ?`, note, id)
		return err
	}
	if color != "" {
		_, err := s.db.Exec(`UPDATE bookmarks SET color = ? WHERE id = ?`, color, id)
		return err
	}
	return nil
}

func (s *Store) CreateBookmark(b Bookmark) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO bookmarks (work_id, book_id, type, chapter_idx, position_secs, start_word, end_word, text_snippet, note, color)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, b.WorkID, b.BookID, b.Type, b.ChapterIdx, b.PositionSecs, b.StartWord, b.EndWord, b.TextSnippet, b.Note, b.Color)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListBookmarks(workID int64) ([]Bookmark, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, book_id, type, chapter_idx, position_secs, start_word, end_word, text_snippet, note, color, created_at
		FROM bookmarks WHERE work_id = ? ORDER BY created_at DESC
	`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bookmarks []Bookmark
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.ID, &b.WorkID, &b.BookID, &b.Type, &b.ChapterIdx,
			&b.PositionSecs, &b.StartWord, &b.EndWord, &b.TextSnippet, &b.Note, &b.Color, &b.CreatedAt); err != nil {
			return nil, err
		}
		bookmarks = append(bookmarks, b)
	}
	return bookmarks, rows.Err()
}

func (s *Store) DeleteBookmark(id int64) error {
	_, err := s.db.Exec(`DELETE FROM bookmarks WHERE id = ?`, id)
	return err
}

type Job struct {
	ID          string  `json:"id"`
	WorkID      int64   `json:"work_id"`
	Type        string  `json:"type"`
	Status      string  `json:"status"`
	Progress    float64 `json:"progress"`
	CurrentStep string  `json:"current_step"`
	Error       string  `json:"error,omitempty"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func (s *Store) UpsertJob(j Job) error {
	_, err := s.db.Exec(`
		INSERT INTO jobs (id, work_id, type, status, progress, current_step, error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			status = excluded.status,
			progress = excluded.progress,
			current_step = excluded.current_step,
			error = excluded.error,
			updated_at = CURRENT_TIMESTAMP
	`, j.ID, j.WorkID, j.Type, j.Status, j.Progress, j.CurrentStep, j.Error)
	return err
}

func (s *Store) ListJobs() ([]Job, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, type, status, progress, current_step, error, created_at, updated_at
		FROM jobs ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.WorkID, &j.Type, &j.Status, &j.Progress,
			&j.CurrentStep, &j.Error, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) GetJob(id string) (*Job, error) {
	var j Job
	err := s.db.QueryRow(`
		SELECT id, work_id, type, status, progress, current_step, error, created_at, updated_at
		FROM jobs WHERE id = ?
	`, id).Scan(&j.ID, &j.WorkID, &j.Type, &j.Status, &j.Progress,
		&j.CurrentStep, &j.Error, &j.CreatedAt, &j.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// sqliteTimeFmt matches SQLite's CURRENT_TIMESTAMP (UTC). We write log
// timestamps in this exact format so lexicographic comparison against a
// `since` bound is also chronological.
const sqliteTimeFmt = "2006-01-02 15:04:05"

// LogFilter narrows a QueryLogs read. All fields are optional; the zero
// value returns the most recent Limit entries across everything.
type LogFilter struct {
	MinLevel  applog.Level // entries at or above this severity
	Component string       // exact component match
	JobID     string       // exact job id match
	WorkID    int64        // exact work id match (0 = no filter)
	Query     string       // case-insensitive substring of the message
	Since     time.Time    // only entries at/after this time
	Limit     int          // default 200, capped at 2000
}

// InsertLogs batch-writes structured entries. Implements applog.Store.
// Fields maps are JSON-encoded; an empty/unencodable map stores "".
func (s *Store) InsertLogs(entries []applog.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO logs (ts, level, component, job_id, work_id, message, fields)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		fields := ""
		if len(e.Fields) > 0 {
			if b, err := json.Marshal(e.Fields); err == nil {
				fields = string(b)
			}
		}
		ts := e.Time
		if ts.IsZero() {
			ts = time.Now()
		}
		level := e.Level
		if level == "" {
			level = applog.LevelInfo
		}
		if _, err := stmt.Exec(ts.UTC().Format(sqliteTimeFmt), string(level),
			e.Component, e.JobID, e.WorkID, e.Message, fields); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QueryLogs reads the recent window newest-first, applying the filter.
func (s *Store) QueryLogs(f LogFilter) ([]applog.Entry, error) {
	where := []string{"1=1"}
	var args []any

	if f.MinLevel != "" {
		levels := applog.LevelsAtOrAbove(f.MinLevel)
		ph := make([]string, len(levels))
		for i, l := range levels {
			ph[i] = "?"
			args = append(args, l)
		}
		where = append(where, "level IN ("+strings.Join(ph, ",")+")")
	}
	if f.Component != "" {
		where = append(where, "component = ?")
		args = append(args, f.Component)
	}
	if f.JobID != "" {
		where = append(where, "job_id = ?")
		args = append(args, f.JobID)
	}
	if f.WorkID > 0 {
		where = append(where, "work_id = ?")
		args = append(args, f.WorkID)
	}
	if f.Query != "" {
		where = append(where, "message LIKE ?")
		args = append(args, "%"+f.Query+"%")
	}
	if !f.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.Since.UTC().Format(sqliteTimeFmt))
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT ts, level, component, job_id, work_id, message, fields
		FROM logs WHERE `+strings.Join(where, " AND ")+`
		ORDER BY ts DESC, id DESC LIMIT ?
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]applog.Entry, 0, limit)
	for rows.Next() {
		var (
			e      applog.Entry
			ts     time.Time // modernc decodes the DATETIME column to time.Time
			level  string
			fields string
		)
		if err := rows.Scan(&ts, &level, &e.Component, &e.JobID, &e.WorkID, &e.Message, &fields); err != nil {
			return nil, err
		}
		e.Time = ts.UTC()
		e.Level = applog.Level(level)
		if fields != "" {
			_ = json.Unmarshal([]byte(fields), &e.Fields)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DistinctLogComponents lists the components currently present, for the
// console's filter dropdown.
func (s *Store) DistinctLogComponents() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT component FROM logs WHERE component <> '' ORDER BY component`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneLogs deletes entries older than before. Implements applog.Store.
func (s *Store) PruneLogs(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM logs WHERE ts < ?`, before.UTC().Format(sqliteTimeFmt))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CleanupOrphanedBooks removes DB entries where the file no longer exists on disk.
func (s *Store) CleanupOrphanedBooks() (int, error) {
	books, err := s.ListBooks()
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, b := range books {
		// Skip virtual/generated paths that aren't real files
		if strings.HasPrefix(b.Path, "generated://") {
			continue
		}
		if _, err := os.Stat(b.Path); os.IsNotExist(err) {
			// Cascade to the book's content so we don't leave orphaned
			// chunks/paragraphs/chapters (the embed pass only walks live
			// books, so orphaned chunks would stay unembedded forever and
			// inflate the coverage gap).
			s.db.Exec(`DELETE FROM chunks WHERE book_id = ?`, b.ID)
			s.db.Exec(`DELETE FROM paragraphs WHERE book_id = ?`, b.ID)
			s.db.Exec(`DELETE FROM chapters WHERE book_id = ?`, b.ID)
			s.db.Exec(`DELETE FROM books WHERE id = ?`, b.ID)
			removed++
		}
	}
	return removed, nil
}

// CleanupOrphanedRows deletes chunks/paragraphs/chapters whose owning book no
// longer exists — debris from earlier book deletions that didn't cascade
// (pre-fix CleanupOrphanedBooks). Idempotent; returns the row count removed.
func (s *Store) CleanupOrphanedRows() (int64, error) {
	var total int64
	for _, tbl := range []string{"chunks", "paragraphs", "chapters"} {
		res, err := s.db.Exec(`DELETE FROM ` + tbl + ` WHERE book_id NOT IN (SELECT id FROM books)`)
		if err != nil {
			return total, fmt.Errorf("cleanup orphaned %s: %w", tbl, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

type SyncTimestamp struct {
	Start float64 `json:"s"`
	End   float64 `json:"e"`
	Word  string  `json:"w"`
}

func (s *Store) SaveSyncData(workID, audioBookID int64, chapterIdx int, timestamps string) error {
	_, err := s.db.Exec(`
		INSERT INTO sync_data (work_id, audio_book_id, chapter_idx, timestamps)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(work_id, audio_book_id, chapter_idx) DO UPDATE SET
			timestamps = excluded.timestamps,
			created_at = CURRENT_TIMESTAMP
	`, workID, audioBookID, chapterIdx, timestamps)
	return err
}

func (s *Store) GetSyncData(workID, audioBookID int64, chapterIdx int) (string, error) {
	var timestamps string
	err := s.db.QueryRow(`
		SELECT timestamps FROM sync_data
		WHERE work_id = ? AND audio_book_id = ? AND chapter_idx = ?
	`, workID, audioBookID, chapterIdx).Scan(&timestamps)
	if err == sql.ErrNoRows {
		return "[]", nil
	}
	return timestamps, err
}

// SyncRow is one chapter's word-timing blob for a given audio book. Returned
// by ListSyncForWork so the .abook exporter can carve every sync row for a
// work in one query.
type SyncRow struct {
	ID          int64
	AudioBookID int64
	ChapterIdx  int
	Timestamps  string
}

// ListSyncForWork returns every sync_data row for a work (all audio books,
// all chapters). Used by the book.db carve.
func (s *Store) ListSyncForWork(workID int64) ([]SyncRow, error) {
	rows, err := s.db.Query(`
		SELECT id, audio_book_id, chapter_idx, timestamps
		FROM sync_data WHERE work_id = ? ORDER BY audio_book_id, chapter_idx
	`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncRow
	for rows.Next() {
		var sr SyncRow
		if err := rows.Scan(&sr.ID, &sr.AudioBookID, &sr.ChapterIdx, &sr.Timestamps); err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

// --- Alignments CRUD ---

// SaveAlignment upserts a pairwise alignment between two source books.
// The pairs blob should be pre-serialized JSON of []AlignmentPair.
func (s *Store) SaveAlignment(a Alignment) error {
	_, err := s.db.Exec(`
		INSERT INTO alignments (work_id, from_book_id, to_book_id, unit, confidence, method, pairs, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(from_book_id, to_book_id, unit) DO UPDATE SET
			confidence = excluded.confidence,
			method     = excluded.method,
			pairs      = excluded.pairs,
			updated_at = CURRENT_TIMESTAMP
	`, a.WorkID, a.FromBookID, a.ToBookID, a.Unit, a.Confidence, a.Method, a.Pairs)
	return err
}

// GetAlignment returns the alignment between two specific books for a unit,
// or nil if none exists. Checks both directions (from→to and to→from).
func (s *Store) GetAlignment(bookA, bookB int64, unit string) (*Alignment, error) {
	var a Alignment
	err := s.db.QueryRow(`
		SELECT id, work_id, from_book_id, to_book_id, unit, confidence, method, pairs, created_at, updated_at
		FROM alignments
		WHERE ((from_book_id = ? AND to_book_id = ?) OR (from_book_id = ? AND to_book_id = ?))
		  AND unit = ?
	`, bookA, bookB, bookB, bookA, unit).Scan(
		&a.ID, &a.WorkID, &a.FromBookID, &a.ToBookID, &a.Unit,
		&a.Confidence, &a.Method, &a.Pairs, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListAlignmentsForWork returns all alignments associated with a work.
func (s *Store) ListAlignmentsForWork(workID int64) ([]Alignment, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, from_book_id, to_book_id, unit, confidence, method, pairs, created_at, updated_at
		FROM alignments WHERE work_id = ?
	`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alignment
	for rows.Next() {
		var a Alignment
		if err := rows.Scan(&a.ID, &a.WorkID, &a.FromBookID, &a.ToBookID, &a.Unit,
			&a.Confidence, &a.Method, &a.Pairs, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// BestAlignmentByWork returns, for every work that has at least one alignment
// row, the (method, confidence) of its highest-confidence row. Lets the work
// list surface a coverage indicator without loading every work's pairs blob.
type BestAlignment struct {
	Method     string
	Confidence float64
}

func (s *Store) BestAlignmentByWork() (map[int64]BestAlignment, error) {
	rows, err := s.db.Query(`
		SELECT work_id, method, confidence
		FROM alignments
		ORDER BY work_id, confidence DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]BestAlignment{}
	for rows.Next() {
		var wid int64
		var ba BestAlignment
		if err := rows.Scan(&wid, &ba.Method, &ba.Confidence); err != nil {
			return nil, err
		}
		if _, seen := out[wid]; !seen { // ORDER BY guarantees this is the best
			out[wid] = ba
		}
	}
	return out, rows.Err()
}

// ListAlignmentsForBook returns all alignments where the given book is either
// the from or to side. Used by ResolvePath to build a composition chain.
func (s *Store) ListAlignmentsForBook(bookID int64) ([]Alignment, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, from_book_id, to_book_id, unit, confidence, method, pairs, created_at, updated_at
		FROM alignments
		WHERE from_book_id = ? OR to_book_id = ?
	`, bookID, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alignment
	for rows.Next() {
		var a Alignment
		if err := rows.Scan(&a.ID, &a.WorkID, &a.FromBookID, &a.ToBookID, &a.Unit,
			&a.Confidence, &a.Method, &a.Pairs, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAlignment removes a specific alignment by ID.
func (s *Store) DeleteAlignment(id int64) error {
	_, err := s.db.Exec(`DELETE FROM alignments WHERE id = ?`, id)
	return err
}

// DeleteAlignmentsForBook removes all alignments involving a given book.
// Used when a source is deleted or re-generated.
func (s *Store) DeleteAlignmentsForBook(bookID int64) error {
	_, err := s.db.Exec(`DELETE FROM alignments WHERE from_book_id = ? OR to_book_id = ?`, bookID, bookID)
	return err
}

func (s *Store) DeleteJob(id string) error {
	_, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	return err
}

// MergeWorks moves all content from sourceID into targetID, then deletes the
// source work. Books, chapter_links, sync_data, bookmarks, and playback
// positions are all reassigned. Chapters stay attached to their books (which
// move via the book's work_id).
func (s *Store) MergeWorks(targetID, sourceID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Tables with unique constraints on work_id — drop source rows that
	// would conflict, keeping the target's existing data.
	conflictTables := []string{"playback_positions", "sync_data"}
	for _, tbl := range conflictTables {
		tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE work_id = ? AND work_id IN (SELECT work_id FROM %s WHERE work_id = ?)`, tbl, tbl), sourceID, targetID)
	}

	moves := []string{
		`UPDATE books SET work_id = ? WHERE work_id = ?`,
		`UPDATE chapter_links SET work_id = ? WHERE work_id = ?`,
		`UPDATE OR IGNORE sync_data SET work_id = ? WHERE work_id = ?`,
		`UPDATE bookmarks SET work_id = ? WHERE work_id = ?`,
		`UPDATE OR IGNORE playback_positions SET work_id = ? WHERE work_id = ?`,
		`UPDATE jobs SET work_id = ? WHERE work_id = ?`,
	}
	for _, q := range moves {
		if _, err := tx.Exec(q, targetID, sourceID); err != nil {
			return fmt.Errorf("merge step failed: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM works WHERE id = ?`, sourceID); err != nil {
		return fmt.Errorf("delete source work: %w", err)
	}
	return tx.Commit()
}

// DeleteWork removes a work and all its associated data.
func (s *Store) DeleteWork(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete in dependency order.
	bookIDs := []int64{}
	rows, _ := tx.Query(`SELECT id FROM books WHERE work_id = ?`, id)
	for rows.Next() {
		var bid int64
		rows.Scan(&bid)
		bookIDs = append(bookIDs, bid)
	}
	rows.Close()
	for _, bid := range bookIDs {
		tx.Exec(`DELETE FROM chapters WHERE book_id = ?`, bid)
		tx.Exec(`DELETE FROM chunks WHERE book_id = ?`, bid)
	}
	tx.Exec(`DELETE FROM books WHERE work_id = ?`, id)
	tx.Exec(`DELETE FROM chapter_links WHERE work_id = ?`, id)
	tx.Exec(`DELETE FROM sync_data WHERE work_id = ?`, id)
	tx.Exec(`DELETE FROM bookmarks WHERE work_id = ?`, id)
	tx.Exec(`DELETE FROM playback_positions WHERE work_id = ?`, id)
	tx.Exec(`DELETE FROM jobs WHERE work_id = ?`, id)
	// Cascade-delete chat sessions + their messages. ON DELETE CASCADE
	// only fires when foreign_keys=ON, which we don't enable, so we
	// drop dependents explicitly.
	tx.Exec(`DELETE FROM qa_messages WHERE session_id IN (SELECT id FROM qa_sessions WHERE work_id = ?)`, id)
	tx.Exec(`DELETE FROM qa_sessions WHERE work_id = ?`, id)
	tx.Exec(`DELETE FROM works WHERE id = ?`, id)
	return tx.Commit()
}
