package db

import (
	"crypto/rand"
	"encoding/base64"
	"time"
)

// DefaultSessionTTL is how long a login (or QR-paired) token stays
// valid. Tokens live in the auth_sessions table so they survive
// server restarts — important because this server restarts often
// (updates, job auto-resume) and a 30-day token would otherwise be
// lost on every bounce.
const DefaultSessionTTL = 30 * 24 * time.Hour

// NewSessionToken mints a 32-byte base64url token. Caller persists it
// via CreateAuthSession.
func NewSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// CreateAuthSession stores a token for username with the given TTL.
// Pass DefaultSessionTTL for the standard 30-day window.
func (s *Store) CreateAuthSession(token, username string, ttl time.Duration) error {
	expires := time.Now().Add(ttl).UTC()
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO auth_sessions (token, username, expires_at) VALUES (?, ?, ?)`,
		token, username, expires,
	)
	return err
}

// ValidateAuthSession returns the username for a non-expired token.
// ok is false when the token is unknown or expired. Expired rows are
// best-effort deleted on the way out.
func (s *Store) ValidateAuthSession(token string) (username string, ok bool) {
	if token == "" {
		return "", false
	}
	var expires time.Time
	err := s.db.QueryRow(
		`SELECT username, expires_at FROM auth_sessions WHERE token = ?`, token,
	).Scan(&username, &expires)
	if err != nil {
		return "", false
	}
	if time.Now().After(expires) {
		s.db.Exec(`DELETE FROM auth_sessions WHERE token = ?`, token)
		return "", false
	}
	return username, true
}

// DeleteAuthSession removes one token (logout). No error if absent.
func (s *Store) DeleteAuthSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM auth_sessions WHERE token = ?`, token)
	return err
}

// PurgeExpiredAuthSessions deletes all expired rows. Called on boot;
// per-request validation also drops expired tokens lazily.
func (s *Store) PurgeExpiredAuthSessions() error {
	_, err := s.db.Exec(`DELETE FROM auth_sessions WHERE expires_at < ?`, time.Now().UTC())
	return err
}
