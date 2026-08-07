package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/db"
	"golang.org/x/crypto/bcrypt"
)

// Household reader management (multi-user HTTP surface). The schema, scoping and
// ownership checks already exist; these endpoints finish the feature so a second
// person in the house can be added without a database operation. All are behind
// authMiddleware — reader accounts only mean anything when login is on.

type readerRow struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsYou    bool   `json:"is_you"`
	Data     any    `json:"data"` // db.UserDataCounts — what a removal would delete
}

// POST /api/readers/enable-sharing — the first-run "share your library" step.
// A person reading alone never sees accounts; this is where multi-user first
// becomes real, framed as setting up THEIR sign-in (they become reader 1, owning
// the history they already have), not administering user accounts. It enables
// login, creates reader 1 immediately (no restart), and keeps them signed in so
// the flow never bounces them to a login screen for a password they just set.
func (s *Server) handleEnableSharing(w http.ResponseWriter, r *http.Request) {
	if s.authEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sharing is already set up"})
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "your name is required"})
		return
	}
	if len(req.Password) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 6 characters"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if err := s.store.UpsertPrimaryUser(req.Name, string(hash)); err != nil {
		writeServerError(w, r, err)
		return
	}
	// Enable auth: the hash is the single source of truth (authEnabled), the
	// username mirrors it so the startup seed stays consistent.
	if err := s.store.SetSetting("auth_username", req.Name); err != nil {
		writeServerError(w, r, err)
		return
	}
	if err := s.store.SetSetting("auth_password_hash", string(hash)); err != nil {
		writeServerError(w, r, err)
		return
	}
	// Keep them signed in seamlessly — no bounce to a login screen mid-flow.
	if token, err := db.NewSessionToken(); err == nil {
		if err := s.store.CreateAuthSession(token, 1, req.Name, db.DefaultSessionTTL); err == nil {
			s.setSessionCookie(w, r, token, db.DefaultSessionTTL)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": req.Name})
}

// GET /api/users — list readers with the per-reader data each holds, so the
// manage-readers UI can show what removing one would destroy.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	me := userIDFromContext(r)
	out := make([]readerRow, 0, len(users))
	for _, u := range users {
		out = append(out, readerRow{
			ID:       u.ID,
			Username: u.Username,
			IsYou:    u.ID == me,
			Data:     s.store.UserDataCounts(u.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/users — add a reader {username, password}. Each reader logs in with
// their own credentials and gets their own positions/bookmarks/Q&A.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a name is required"})
		return
	}
	if len(req.Password) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 6 characters"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	id, err := s.store.CreateUser(req.Username, string(hash))
	if err != nil {
		// UNIQUE(username) — the only expected failure.
		writeJSON(w, http.StatusConflict, map[string]string{"error": "that name is already taken"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "username": req.Username})
}

// DELETE /api/users/{id} — remove a reader and ALL their data. Guards: you can't
// remove yourself (that would lock you out mid-session), and you can't remove the
// last reader (someone has to own the library). The destructive confirmation —
// naming what's lost — is the caller's responsibility and the UI shows it.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid reader id"})
		return
	}
	if id == userIDFromContext(r) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "you can't remove yourself"})
		return
	}
	if !s.store.UserExists(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such reader"})
		return
	}
	users, err := s.store.ListUsers()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if len(users) <= 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "can't remove the only reader"})
		return
	}
	if err := s.store.DeleteUser(id); err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": id})
}
