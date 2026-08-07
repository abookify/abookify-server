package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// The first-run "set up sharing" step must, in one call and with no restart:
// turn on login, create reader 1 as the person themselves (so their existing
// history stays theirs), and let them log in immediately. This is the human-
// facing last piece of household multi-user, so it gets a real end-to-end lock.
func TestEnableSharingFirstRun(t *testing.T) {
	srv, store, _ := newTestServer(t)
	if srv.authEnabled() {
		t.Fatal("fresh server should start with auth off")
	}

	// short password rejected
	rec := httptest.NewRecorder()
	srv.handleEnableSharing(rec, httptest.NewRequest("POST", "/api/readers/enable-sharing",
		strings.NewReader(`{"name":"Alex","password":"short"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short password should 400, got %d", rec.Code)
	}

	// happy path
	rec = httptest.NewRecorder()
	srv.handleEnableSharing(rec, httptest.NewRequest("POST", "/api/readers/enable-sharing",
		strings.NewReader(`{"name":"Alex","password":"secret123"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("enable-sharing should 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !srv.authEnabled() {
		t.Fatal("auth should be enabled after setup")
	}
	// reader 1 is the person, and their password verifies (they can log in now)
	id, hash, ok := store.GetUserByUsername("Alex")
	if !ok || id != 1 {
		t.Fatalf("Alex should be reader id 1, got id=%d ok=%v", id, ok)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("secret123")) != nil {
		t.Fatal("reader 1's password should verify")
	}
	// they're signed in immediately — a session cookie was set, no login bounce
	if len(rec.Result().Cookies()) == 0 {
		t.Error("expected a session cookie so the person stays signed in")
	}

	// second call is refused — sharing is already set up
	rec = httptest.NewRecorder()
	srv.handleEnableSharing(rec, httptest.NewRequest("POST", "/api/readers/enable-sharing",
		strings.NewReader(`{"name":"Someone","password":"another1"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("second enable-sharing should 400 (already set up), got %d", rec.Code)
	}
}
