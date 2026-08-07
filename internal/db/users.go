package db

import "database/sql"

// Household multi-user. One library, many readers; each reader has their own
// playback position, bookmarks and Q&A (scoped by user_id on those tables). A
// single-credential (#197) install is migrated to user id=1 by migrate().

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
}

// GetUserByUsername returns the id + password hash for a username (for login).
// ok is false when no such user exists.
func (s *Store) GetUserByUsername(username string) (id int64, passwordHash string, ok bool) {
	err := s.db.QueryRow(
		`SELECT id, password_hash FROM users WHERE username = ?`, username,
	).Scan(&id, &passwordHash)
	if err == sql.ErrNoRows {
		return 0, "", false
	}
	if err != nil {
		return 0, "", false
	}
	return id, passwordHash, true
}

// CreateUser inserts a reader with a pre-hashed password and returns its id.
// Fails (UNIQUE) if the username is taken.
func (s *Store) CreateUser(username, passwordHash string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO users (username, password_hash) VALUES (?, ?)`, username, passwordHash,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListUsers returns all readers (no password material), for the add/remove-reader UI.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, username, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpsertPrimaryUser makes (or renames) reader id 1 with the given credentials.
// The first reader is always id 1 — the same default user_id that solo reading
// already wrote positions/bookmarks/Q&A under — so turning on sharing keeps a
// person's existing history theirs instead of stranding it under a phantom user.
// Runs immediately (no restart), unlike migrate()'s startup seed.
func (s *Store) UpsertPrimaryUser(username, passwordHash string) error {
	_, err := s.db.Exec(
		`INSERT INTO users (id, username, password_hash) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET username = excluded.username, password_hash = excluded.password_hash`,
		username, passwordHash)
	return err
}

// UserExists reports whether a reader id is present.
func (s *Store) UserExists(userID int64) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&n)
	return n > 0
}

// UserDataCounts is the per-reader data a removal would destroy — surfaced in
// the confirmation so nobody deletes a reader without seeing what goes with them.
type UserDataCounts struct {
	Positions  int `json:"positions"`
	Bookmarks  int `json:"bookmarks"`
	QASessions int `json:"qa_sessions"`
}

// UserDataCounts returns the reader's reading positions, bookmarks, and Q&A
// conversation counts.
func (s *Store) UserDataCounts(userID int64) UserDataCounts {
	var c UserDataCounts
	s.db.QueryRow(`SELECT COUNT(*) FROM playback_positions WHERE user_id = ?`, userID).Scan(&c.Positions)
	s.db.QueryRow(`SELECT COUNT(*) FROM bookmarks WHERE user_id = ?`, userID).Scan(&c.Bookmarks)
	s.db.QueryRow(`SELECT COUNT(*) FROM qa_sessions WHERE user_id = ?`, userID).Scan(&c.QASessions)
	return c
}

// DeleteUser removes a reader and ALL their per-reader data — reading positions,
// playback history, bookmarks, Q&A conversations, and login sessions. This is
// irreversible; the caller is responsible for confirming intent. Runs in one
// transaction so a reader is never left half-deleted.
func (s *Store) DeleteUser(userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// qa_messages hang off qa_sessions by session_id.
	if _, err := tx.Exec(
		`DELETE FROM qa_messages WHERE session_id IN (SELECT id FROM qa_sessions WHERE user_id = ?)`, userID); err != nil {
		return err
	}
	for _, t := range []string{"qa_sessions", "bookmarks", "playback_events", "playback_positions", "auth_sessions"} {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE user_id = ?`, userID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}
