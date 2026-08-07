package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

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
