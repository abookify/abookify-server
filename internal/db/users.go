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
