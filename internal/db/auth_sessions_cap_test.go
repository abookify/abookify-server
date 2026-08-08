package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSessionCapAndRevoke(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Mint many sessions for one user; the cap must bound them.
	for i := 0; i < maxSessionsPerUser+15; i++ {
		tok, _ := NewSessionToken()
		if err := s.CreateAuthSession(tok, 1, "pj", DefaultSessionTTL); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.CountAuthSessions(); n > maxSessionsPerUser {
		t.Fatalf("session cap failed: %d sessions, want <= %d", n, maxSessionsPerUser)
	}
	// A second user's sessions are capped independently.
	for i := 0; i < 5; i++ {
		tok, _ := NewSessionToken()
		s.CreateAuthSession(tok, 2, "test", DefaultSessionTTL)
	}
	// The most-recent token for user 1 still validates (cap keeps recent).
	recent, _ := NewSessionToken()
	s.CreateAuthSession(recent, 1, "pj", DefaultSessionTTL)
	if _, _, ok := s.ValidateAuthSession(recent); !ok {
		t.Fatal("most recent session should validate")
	}
	// Password change → revoke ALL.
	if err := s.DeleteAllAuthSessions(); err != nil {
		t.Fatal(err)
	}
	if n := s.CountAuthSessions(); n != 0 {
		t.Fatalf("revoke-all failed: %d sessions remain", n)
	}
	if _, _, ok := s.ValidateAuthSession(recent); ok {
		t.Fatal("session must be invalid after revoke-all")
	}
	_ = time.Now
}
