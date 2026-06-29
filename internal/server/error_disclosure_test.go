package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// writeServerError must return a generic 500 with no internal detail on the
// wire — the underlying error goes to the logs, not the response body.
func TestWriteServerErrorIsGeneric(t *testing.T) {
	rec := httptest.NewRecorder()
	secret := "pq: relation \"users\" does not exist at /home/pj/secret/path"
	writeServerError(rec, httptest.NewRequest("GET", "/api/x", nil), errors.New(secret))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) || strings.Contains(body, "/home/pj") || strings.Contains(body, "pq:") {
		t.Errorf("response body leaked internal error detail: %s", body)
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] != "internal server error" {
		t.Errorf("error body = %q, want generic", out["error"])
	}
}

// /api/setup exposes data_dir/models_dir (local FS paths) only to a trusted
// caller: present when auth is off or the request is authenticated, withheld
// from an unauthenticated caller on an auth-on server.
func TestHandleSetupHidesPathsFromUnauthenticated(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.DataDir, srv.ModelsDir = "/home/pj/.abookify", "/home/pj/.abookify/models"

	call := func(authHeader string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/setup", nil)
		if authHeader != "" {
			req.Header.Set("Authorization", "Bearer "+authHeader)
		}
		rec := httptest.NewRecorder()
		srv.handleSetup(rec, req)
		var b map[string]any
		json.Unmarshal(rec.Body.Bytes(), &b)
		return b
	}

	// Auth OFF (open server) → paths present (the desktop-shell case).
	if b := call(""); b["data_dir"] != "/home/pj/.abookify" {
		t.Errorf("auth-off: data_dir = %v, want exposed", b["data_dir"])
	}

	// Enable auth.
	store.SetSetting("auth_password_hash", "$2a$10$placeholderhashvalueforthetestonly000000000000000000")
	store.SetSetting("auth_username", "pj")

	// Auth ON + unauthenticated → paths withheld, booleans still present.
	b := call("")
	if _, ok := b["data_dir"]; ok {
		t.Errorf("auth-on unauthenticated: data_dir leaked (%v)", b["data_dir"])
	}
	if _, ok := b["models_dir"]; ok {
		t.Errorf("auth-on unauthenticated: models_dir leaked (%v)", b["models_dir"])
	}
	if _, ok := b["needs_setup"]; !ok {
		t.Error("needs_setup should still be present pre-login")
	}

	// Auth ON + valid session → paths exposed.
	tok, _ := db.NewSessionToken()
	store.CreateAuthSession(tok, "pj", db.DefaultSessionTTL)
	if b := call(tok); b["data_dir"] != "/home/pj/.abookify" {
		t.Errorf("auth-on authenticated: data_dir = %v, want exposed", b["data_dir"])
	}
}
