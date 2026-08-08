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

// maxSessionsPerUser caps how many live tokens one reader accumulates. Something
// that re-authenticates on a cadence (a monitor/health check that logs in fresh
// instead of reusing its token) would otherwise mint 30-day tokens forever — 460
// of them were found in the wild. Each CreateAuthSession trims the user back to
// the most-recent N, so accumulation is bounded regardless of the client.
const maxSessionsPerUser = 25

// CreateAuthSession stores a token for a user with the given TTL, then trims that
// user's sessions to the most recent maxSessionsPerUser. Pass DefaultSessionTTL
// for the standard 30-day window. userID scopes the session to a reader
// (multi-user); username is kept for display/back-compat.
func (s *Store) CreateAuthSession(token string, userID int64, username string, ttl time.Duration) error {
	expires := time.Now().Add(ttl).UTC()
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO auth_sessions (token, user_id, username, expires_at) VALUES (?, ?, ?, ?)`,
		token, userID, username, expires,
	); err != nil {
		return err
	}
	// Cap this user's live tokens (bounds the re-auth-every-N-minutes accumulation).
	s.db.Exec(`DELETE FROM auth_sessions WHERE user_id = ? AND token NOT IN (
		SELECT token FROM auth_sessions WHERE user_id = ? ORDER BY expires_at DESC LIMIT ?)`,
		userID, userID, maxSessionsPerUser)
	return nil
}

// DeleteAllAuthSessions revokes EVERY session — used when the password changes,
// so a changed password actually signs everyone out (a session that outlives the
// password it was minted under defeats the point of changing it). The person who
// changed it re-signs-in with the new password.
func (s *Store) DeleteAllAuthSessions() error {
	_, err := s.db.Exec(`DELETE FROM auth_sessions`)
	return err
}

// CountAuthSessions returns how many session rows exist (for health reporting).
func (s *Store) CountAuthSessions() int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n)
	return n
}

// ValidateAuthSession returns the user_id + username for a non-expired token.
// ok is false when the token is unknown or expired. Expired rows are
// best-effort deleted on the way out.
func (s *Store) ValidateAuthSession(token string) (userID int64, username string, ok bool) {
	if token == "" {
		return 0, "", false
	}
	var expires time.Time
	err := s.db.QueryRow(
		`SELECT user_id, username, expires_at FROM auth_sessions WHERE token = ?`, token,
	).Scan(&userID, &username, &expires)
	if err != nil {
		return 0, "", false
	}
	if time.Now().After(expires) {
		s.db.Exec(`DELETE FROM auth_sessions WHERE token = ?`, token)
		return 0, "", false
	}
	return userID, username, true
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
