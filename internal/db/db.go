package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	AudioFiles   []Book         `json:"audio_files,omitempty"`
	TextFiles    []Book         `json:"text_files,omitempty"`
	ChapterLinks []ChapterLink  `json:"chapter_links,omitempty"`
	TotalSize    int64          `json:"total_size"`
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
	// Content is only loaded on demand, not in list responses
	Content string `json:"content,omitempty"`
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
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

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
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			title      TEXT NOT NULL DEFAULT '',
			author     TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

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
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (work_id) REFERENCES works(id)
		);

		CREATE INDEX IF NOT EXISTS idx_books_format ON books(format);
		CREATE INDEX IF NOT EXISTS idx_books_work_id ON books(work_id);
		CREATE INDEX IF NOT EXISTS idx_books_media_type ON books(media_type);

		CREATE TABLE IF NOT EXISTS chapters (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			book_id    INTEGER NOT NULL,
			index_num  INTEGER NOT NULL,
			title      TEXT NOT NULL DEFAULT '',
			src        TEXT NOT NULL DEFAULT '',
			content    TEXT NOT NULL DEFAULT '',
			word_count INTEGER NOT NULL DEFAULT 0,
			start_sec  REAL NOT NULL DEFAULT 0,
			end_sec    REAL NOT NULL DEFAULT 0,
			confidence REAL NOT NULL DEFAULT 0,
			FOREIGN KEY (book_id) REFERENCES books(id),
			UNIQUE(book_id, index_num)
		);

		CREATE INDEX IF NOT EXISTS idx_chapters_book_id ON chapters(book_id);

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

		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		);
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
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migration %q: %w", stmt, err)
		}
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
	return nil
}

type PlaybackPosition struct {
	WorkID       int64   `json:"work_id"`
	BookID       int64   `json:"book_id"`
	FileIndex    int     `json:"file_index"`
	PositionSecs float64 `json:"position_secs"`
	UpdatedAt    string  `json:"updated_at"`
}

func (s *Store) SavePosition(pos PlaybackPosition) error {
	_, err := s.db.Exec(`
		INSERT INTO playback_positions (work_id, book_id, file_index, position_secs, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(work_id) DO UPDATE SET
			book_id = excluded.book_id,
			file_index = excluded.file_index,
			position_secs = excluded.position_secs,
			updated_at = CURRENT_TIMESTAMP
	`, pos.WorkID, pos.BookID, pos.FileIndex, pos.PositionSecs)
	return err
}

func (s *Store) GetPosition(workID int64) (*PlaybackPosition, error) {
	var pos PlaybackPosition
	err := s.db.QueryRow(`
		SELECT work_id, book_id, file_index, position_secs, updated_at
		FROM playback_positions WHERE work_id = ?
	`, workID).Scan(&pos.WorkID, &pos.BookID, &pos.FileIndex, &pos.PositionSecs, &pos.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pos, nil
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
		INSERT INTO books (work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
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
			updated_at = CURRENT_TIMESTAMP
	`, b.WorkID, b.Path, b.Filename, b.Format, b.MediaType, b.SizeBytes, b.Title, b.Author, b.Album, b.Duration, b.Origin, b.Visibility)
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

func (s *Store) AssignBooksToWork(workID int64, bookIDs []int64) error {
	for _, id := range bookIDs {
		if _, err := s.db.Exec(`UPDATE books SET work_id = ? WHERE id = ?`, workID, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListBooks() ([]Book, error) {
	rows, err := s.db.Query(`
		SELECT id, work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, created_at, updated_at
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
			&b.Title, &b.Author, &b.Album, &b.Duration, &b.Origin, &b.Visibility, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

func (s *Store) GetBook(id int64) (*Book, error) {
	var b Book
	err := s.db.QueryRow(`
		SELECT id, work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, created_at, updated_at
		FROM books WHERE id = ?
	`, id).Scan(&b.ID, &b.WorkID, &b.Path, &b.Filename, &b.Format, &b.MediaType, &b.SizeBytes,
		&b.Title, &b.Author, &b.Album, &b.Duration, &b.Origin, &b.Visibility, &b.CreatedAt, &b.UpdatedAt)
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
		SELECT id, title, author, created_at, updated_at FROM works ORDER BY title
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var works []Work
	for rows.Next() {
		var w Work
		if err := rows.Scan(&w.ID, &w.Title, &w.Author, &w.CreatedAt, &w.UpdatedAt); err != nil {
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
		SELECT id, title, author, created_at, updated_at FROM works WHERE id = ?
	`, id).Scan(&w.ID, &w.Title, &w.Author, &w.CreatedAt, &w.UpdatedAt)
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
		SELECT id, work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, created_at, updated_at
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
			&b.Title, &b.Author, &b.Album, &b.Duration, &b.Origin, &b.Visibility, &b.CreatedAt, &b.UpdatedAt); err != nil {
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
		SELECT id, work_id, path, filename, format, media_type, size_bytes, title, author, album, duration, origin, visibility, created_at, updated_at
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
			&b.Title, &b.Author, &b.Album, &b.Duration, &b.Origin, &b.Visibility, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

func (s *Store) InsertChapter(ch Chapter) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO chapters (book_id, index_num, title, src, content, word_count, start_sec, end_sec, confidence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, ch.BookID, ch.Index, ch.Title, ch.Src, ch.Content, ch.WordCount, ch.StartSec, ch.EndSec, ch.Confidence)
	return err
}

// DeleteChaptersByBook removes all chapter rows for a given book.
// Used when re-running chapter detection so we don't leave stale entries.
func (s *Store) DeleteChaptersByBook(bookID int64) error {
	_, err := s.db.Exec(`DELETE FROM chapters WHERE book_id = ?`, bookID)
	return err
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

// GetChapterContent returns a single chapter with its content.
func (s *Store) GetChapterContent(bookID int64, index int) (*Chapter, error) {
	var ch Chapter
	err := s.db.QueryRow(`
		SELECT id, book_id, index_num, title, src, content, word_count, start_sec, end_sec, confidence
		FROM chapters WHERE book_id = ? AND index_num = ?
	`, bookID, index).Scan(&ch.ID, &ch.BookID, &ch.Index, &ch.Title, &ch.Src, &ch.Content, &ch.WordCount, &ch.StartSec, &ch.EndSec, &ch.Confidence)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ch, nil
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
			s.db.Exec(`DELETE FROM books WHERE id = ?`, b.ID)
			removed++
		}
	}
	return removed, nil
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
	tx.Exec(`DELETE FROM works WHERE id = ?`, id)
	return tx.Commit()
}
