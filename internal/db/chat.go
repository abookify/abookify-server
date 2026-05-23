package db

import (
	"database/sql"
	"strings"
	"time"
)

type QASession struct {
	ID        int64     `json:"id"`
	WorkID    int64     `json:"work_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type QAMessage struct {
	ID            int64     `json:"id"`
	SessionID     int64     `json:"session_id"`
	Role          string    `json:"role"` // "user" | "assistant"
	Content       string    `json:"content"`
	CitationsJSON string    `json:"-"`
	// ScopeJSON is the marshalled library.QueryScope that produced
	// this turn (user messages only). Empty = whole book / default.
	// Stored as opaque JSON so db package needn't import library.
	ScopeJSON     string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Store) CreateSession(workID int64, title string) (int64, error) {
	if title == "" {
		title = "New chat"
	}
	res, err := s.db.Exec(
		`INSERT INTO qa_sessions (work_id, title) VALUES (?, ?)`,
		workID, title,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListSessions(workID int64) ([]QASession, error) {
	rows, err := s.db.Query(
		`SELECT id, work_id, title, created_at, updated_at
		   FROM qa_sessions WHERE work_id = ? ORDER BY updated_at DESC, id DESC`,
		workID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QASession
	for rows.Next() {
		var ss QASession
		if err := rows.Scan(&ss.ID, &ss.WorkID, &ss.Title, &ss.CreatedAt, &ss.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

func (s *Store) GetSession(id int64) (*QASession, error) {
	var ss QASession
	err := s.db.QueryRow(
		`SELECT id, work_id, title, created_at, updated_at
		   FROM qa_sessions WHERE id = ?`, id,
	).Scan(&ss.ID, &ss.WorkID, &ss.Title, &ss.CreatedAt, &ss.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ss, nil
}

func (s *Store) RenameSession(id int64, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "New chat"
	}
	_, err := s.db.Exec(
		`UPDATE qa_sessions SET title = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		title, id,
	)
	return err
}

func (s *Store) DeleteSession(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM qa_messages WHERE session_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM qa_sessions WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendMessage adds one message to a session and bumps the session's
// updated_at so it floats to the top of the list. scopeJSON is the
// marshalled library.QueryScope that produced this turn (empty for
// assistant rows or whole-book user turns).
func (s *Store) AppendMessage(sessionID int64, role, content, citationsJSON, scopeJSON string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`INSERT INTO qa_messages (session_id, role, content, citations_json, scope_json) VALUES (?, ?, ?, ?, ?)`,
		sessionID, role, content, citationsJSON, scopeJSON,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// strftime with %f gives millisecond precision so two AppendMessages
	// in the same second still produce distinct updated_at values — the
	// session list orders by this for "most-recently-active first".
	if _, err := tx.Exec(
		`UPDATE qa_sessions SET updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now') WHERE id = ?`,
		sessionID,
	); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) ListMessages(sessionID int64) ([]QAMessage, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, role, content, citations_json, scope_json, created_at
		   FROM qa_messages WHERE session_id = ? ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QAMessage
	for rows.Next() {
		var m QAMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CitationsJSON, &m.ScopeJSON, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
